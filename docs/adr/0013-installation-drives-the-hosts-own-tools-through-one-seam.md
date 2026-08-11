# Installation drives the host's own tools through one seam

Installing dokkup means creating a system user, putting it in the `dokku` group,
writing a sudoers rule, making a data directory owned by that user, and writing
one nginx server block. None of that is Dokku and none of it is systemd, so
`internal/dokku` and `internal/service` have nothing to say about it.
`internal/host` is where it goes: the programs are `groupadd`, `useradd`,
`usermod`, `userdel`, `groupdel`, `visudo`, `runuser`, `sudo` and `nginx`, and
they are invoked from one file, as argument vectors, with no shell.

ADR-0012 said systemd was the second thing dokkup executes and that the intent
was that there would never be a third. This is the third, and pretending
otherwise would have meant scattering `exec.Command` through `internal/cli` —
which is the thing that ADR forbids for a reason that has not changed: the
property worth having is that every process dokkup spawns can be found by
reading one file. What that ADR should have said, and what this one says
instead, is that a new family of subprocesses needs a seam and a decision
recorded, not that there is a fixed number of them.

Writing `/etc/passwd`, `/etc/group` and `/etc/shadow` directly was the
alternative to shelling out at all, and it is worse than it looks. Those files
are held under `lckpwdf`, the uid range a system account may take comes from
`/etc/login.defs`, and the rules about what `userdel` may remove alongside a
user are not obvious — it takes the primary group with it only when that group
shares the user's name and has no other members. shadow-utils is the interface
to all of that, exactly as `systemctl` is the interface to systemd. Two of the
programs here are validators and could not be replaced in any case: a sudoers
file has to be parsed by `visudo` before sudo sees it, and an nginx server block
has to be parsed by `nginx -t` before nginx reloads onto it.

The line the interface draws is "everything installation cannot do as an
ordinary user". Subprocesses are behind it, and so is `chown`, and so is writing
into `/etc/sudoers.d`. Plain `os.MkdirAll`, `os.WriteFile` and `os.Rename` are
not: they stay ordinary calls in the installer, rooted through
`hostpaths.Rooted`, so that most of installation runs against a `t.TempDir()`
without root, a container or a Dokku host.

## Consequences

The exit statuses of these programs are the interface, and they are encoded once
with the measurement that established each of them. `groupadd` is idempotent
only with `-f`; plain `groupadd` exits 9 the second time. `useradd`'s exit 9 is
success, because it means the account is already there. `userdel`'s exit 6 is
success and its exit 8 is not — 8 means a process is still running as the user,
which is why removal stops the service first and reports rather than forces.
`groupdel`'s exit 6 is the *ordinary* path, because `userdel` has usually
already taken the group. Every one of those was run rather than read.

`--force` is never passed to `userdel` or `groupdel`, and a test asserts it
appears in no argument vector the suite records. It would turn each of those
failures into a success while leaving the damage: orphaned processes under a uid
the host is free to hand out again, and a dangling numeric gid on files.

The sudoers rule is proved rather than assumed. Installation runs the service
user's own `sudo -n -u dokku dokku version` before it reports success, because
a rule with the wrong mode, the wrong owner or the wrong runas is written
without complaint and only fails later, from inside a service, as a health
endpoint that says the host has no applications.

Growth is on the same terms as ADR-0003 and ADR-0012: a method is added when a
feature needs it, and to the fake in the same change. The fourth family of
programs — whatever it turns out to be — needs its own ADR, and the question it
has to answer is not "may we shell out" but "why does this belong outside the
three seams that already exist".
