# systemd is the second and last thing dokkup shells out to

`dokkup update` replaces the running binary, and then has to restart the service
and find out whether it came back, which means invoking `systemctl`. Every such
invocation goes through `internal/service`, which also holds the text of the
unit. ADR-0003 says `DokkuClient` is the only seam; that rule was written about
Dokku and still holds unchanged. systemd is the second thing dokkup executes,
and the intent is that there is never a third.

There is a seam rather than an `exec.Command` at each place that needs a
restart, because the property worth having is that every process dokkup spawns
can be found by reading one file. Invocations are built as argument vectors and
run without a shell, exactly as in `internal/dokku`, and each one is bounded by
a timeout, since an updater waiting on systemd forever is worse than one that
reports a stuck unit. Tests get a fake that records what was asked in order, so
a test can assert that a failed update restarted the unit twice rather than
hoping it did, and the suite needs no service manager to run.

Talking to systemd over D-Bus was the alternative and was rejected on cost. It
means a dependency and a great deal of surface in exchange for what dokkup
actually wants, which is to restart one unit and ask whether it is running.
`systemctl` is on every host that has systemd, and its exit statuses are a
stable interface — `is-active` answers through the exit status while still
printing the state, which is the one subtlety the seam has to absorb.

The unit text lives in Go rather than in a file installed alongside the binary
because installation, update and removal must not each hold their own idea of
how the service runs. `UnitFile` renders it, and `ListenFromUnit` reads the
address back out of the `ExecStart=` line of the unit that is actually
installed, so `dokkup update` polls the health endpoint of the service as this
host configured it rather than the one it would have configured itself.

## Consequences

A second thing that can be shelled out to is a second thing that can be shelled
out to badly. The rule is that the package stays this small — `Restart`,
`ResetFailed` and `IsActive` — and that a method is added only when a feature
needs it, on the same terms as ADR-0003. A change that wants to enable, mask or
query unit properties is a sign that this decision is being reopened rather than
extended.

`ResetFailed` is there because of how systemd treats a binary that crash-loops:
after `StartLimitBurst` attempts it refuses to start the unit again at all. That
is exactly the state a rollback arrives in, so without clearing it first the
restore would be refused on precisely the hosts the rollback exists for. It is
called best-effort before every restart; a unit that has not failed is unaffected.

`Restart` returns once systemd has finished the job, which means the process was
started, not that it is serving. That distinction is the whole reason
`/api/health` reports the running dokkup's own version: a service that came back
on the old binary satisfies systemd exactly as happily as one that came back on
the new one, so without the version the health gate an update depends on would
mean nothing.

Writing the unit down settled a conflict inside ADR-0005 that had gone unnoticed
while nothing rendered it. That ADR asks for `NoNewPrivileges`, `ProtectHome` and
`ProtectSystem=strict` **and** for a sudoers rule permitting the `dokku` binary,
and those cannot all hold at once. The reason is the subject of this ADR: dokkup
does its work by executing another program, so every sandbox directive in the
unit is inherited by Dokku.

Each was measured in the development environment against Dokku 0.38.7, with an
application really deployed, rather than argued about:

| Directive | Result |
|---|---|
| `NoNewPrivileges=yes` | `sudo: The "no new privileges" flag is set` — dokkup never reaches Dokku |
| `ProtectHome=yes` | `/home/dokku` is `DOKKU_ROOT`; `apps:list` answers "You haven't deployed any applications yet" on a host with apps |
| `ProtectSystem=strict` | `/var/lib/dokku` read-only; `apps:create` fails `read-only file system` |
| `ProtectSystem=full` | reads, writes, a real `git:from-image` deploy and `logs` all succeed |

The dangerous part is that all three fail *silently*. `dokku version` is the only
command `/api/health` runs, and it succeeds under every one of them, so the
service reports itself healthy — and `dokkup update`'s health gate passes —
while the interface shows an empty host. A sandbox that breaks the product
without breaking the health check is worse than no sandbox, because nothing
surfaces it.

So the unit keeps `ProtectSystem=full` and `PrivateTmp`, and drops the other
three; ADR-0005 is amended to match. A test asserts each absence by name, since
adding one back breaks nothing else in the suite.

The general rule this leaves behind: a hardening directive in this unit is a
statement about Dokku as much as about dokkup, and cannot be adopted from a
hardening guide without running Dokku under it first.

## Amended: four more methods, because installing a unit is what needs them

`Manager` has gained `DaemonReload`, `Enable`, `Disable` and `Reload(unit)`. The
Consequences above name enabling a unit as a sign that this decision is being
reopened, and the same paragraph says a method is added only when a feature
needs it. Those point in different directions, and this is the answer:
installation and removal are that feature, and the alternative was a second
place in the tree that runs `systemctl`, which is the exact thing this ADR
exists to prevent. So this is extending, not reopening.

`Enable`, `Disable` and `DaemonReload` are what installing and removing a unit
mean. systemd is otherwise still holding the text of a unit file that has just
changed or gone, and `userdel` exits 8 while a process is still running as the
account — measured on the development environment against a live unit, `userdel:
user zz-probe-svc is currently used by process 342857` — so removal has to stop
the service before it can remove the user it runs as. `Reload` is there because
dokkup writes one file into Dokku's nginx and has to ask nginx to read it; it
takes a unit name because the one caller reloads `nginx` rather than `dokkup`,
and a seam that could not tell the two apart could reload the wrong service.

`systemctl reload nginx` is the form, and both alternatives were rejected on
measurement. `dokku nginx:reload` is exactly `nginx -t` followed by `systemctl
reload nginx` and nothing else, so it would buy nothing while putting a
host-wide action through the app-scoped `DokkuClient` seam. `nginx -s reload`
signals the master process directly: it bypasses systemd and does not run
`nginx -t` first, which is the check that stops a bad file becoming an outage.

The rule that has not changed is that the package holds only `systemctl`, and
that no method arrives without a feature that needs it, or without reaching the
fake in the same change.

## Amended: there is a third thing dokkup shells out to

The title says "last" and the opening paragraph says "the intent is that there
is never a third". There is a third. Installing dokkup means creating a system
user, writing a sudoers rule and writing an nginx server block, which is
`groupadd`, `useradd`, `usermod`, `userdel`, `groupdel`, `visudo`, `runuser`,
`sudo` and `nginx` — none of them Dokku and none of them systemd. They live
behind `internal/host`, and ADR-0013 is where that is argued, including why
writing `/etc/passwd` directly is worse than shelling out and why two of those
programs are validators that could not be replaced in any case.

A count was the wrong shape of promise. What this ADR is protecting is that
every process dokkup spawns can be found by reading one file, and that property
is untouched by a third seam while it would have been destroyed by scattering
`exec.Command` through `internal/cli` in order to keep the number at two. The
rule that replaces the count: a new family of subprocesses needs a seam of its
own and a decision recorded, on the same terms as ADR-0003.
