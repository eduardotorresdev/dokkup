# dokkup

A management layer over a single Dokku instance. dokkup gives operators a web
interface for the day-to-day work that Dokku exposes only through its CLI, and
deliberately owns no deployment state of its own.

## Language

### Boundary

**Dokku Host**:
The single machine running Dokku that a dokkup installation manages. There is
exactly one per installation.
_Avoid_: server, node, instance, cluster

**Dokku**:
The upstream PaaS being managed. It is the authority on everything about
applications; dokkup reads it live and never keeps a second copy.
_Avoid_: backend, engine, upstream

**Own State**:
The data dokkup itself is the authority for: operators, sessions, the audit
trail, deploy records and settings. Everything not in this set belongs to Dokku.
_Avoid_: local state, cache, database

**DokkuClient**:
The single seam through which dokkup invokes Dokku. It abstracts only the
commands dokkup actually uses, and grows as features need it.
_Avoid_: adapter, driver, wrapper, shell

### People

**Operator**:
A person who signs in to dokkup. Operators exist only in dokkup and have no
relationship to Dokku's own SSH users. Every operator can act on the Dokku Host
with root-equivalent power.
_Avoid_: user, account, member, admin

**Owner**:
The single operator who may create, edit and remove other operators. Created
once, during installation, by redeeming the Setup Token.
_Avoid_: superuser, root, first user

**Setup Token**:
A single-use, short-lived secret printed by the installer and redeemed in the
browser to create the Owner. It can be reissued from the CLI only while no Owner
exists.
_Avoid_: invite, bootstrap key, admin token

**Audit Entry**:
An immutable record of an action an Operator took, naming the operator, the
action and its target. It records which configuration key changed, never the
value.
_Avoid_: log, history, event

### Applications

**App**:
An application managed by Dokku. dokkup creates, inspects and removes Apps but
is never their source of truth.
_Avoid_: project, service, deployment

**Process Type**:
A named entry from an App's Procfile, such as `web` or `worker`. Scaling,
healthchecks and log streams are all addressed per Process Type.
_Avoid_: process, dyno, worker, container

**Deploy Record**:
dokkup's stored account of a deploy it triggered itself, including the captured
output. Deploys performed by `git push` leave no Deploy Record, because Dokku
does not retain that output.
_Avoid_: build, release, deployment log

**Managed Volume**:
Persistent storage dokkup created under Dokku's own storage root and can
therefore account for. Host paths outside that root are never mounted by dokkup.
_Avoid_: volume, mount, storage, disk

**App Link**:
A declared connection allowing two Apps to reach each other over a Docker
network, expressed as a relationship between Apps rather than as network
attachment slots. Establishing one requires the Apps to be rebuilt.
_Avoid_: network, attachment, peering, binding

**Check Policy**:
The healthcheck behaviour an operator controls without touching an App's
repository: whether checks gate a deploy, and their timing. Distinct from the
Check Definition.
_Avoid_: healthcheck, check settings

**Check Definition**:
What an App's healthcheck actually probes, declared in `app.json` inside the
App's repository. dokkup can display it but never change it.
_Avoid_: healthcheck, app.json

### Installation

**Installation**:
The act of placing dokkup on a Dokku Host: the binary, its service, its system
user and its data directory. Reversed exactly by removal.
_Avoid_: setup, deployment, provisioning

**Publishing**:
Exposing dokkup at a domain with a valid certificate, as opposed to reaching it
by the host's IP address.
_Avoid_: exposing, serving, hosting

**IP Mode**:
The degraded state in which dokkup is reached by IP address rather than a
published domain. No certificate authority will vouch for it, so dokkup restricts
itself to the Owner alone and says so on every screen.
_Avoid_: insecure mode, local mode, dev mode

**Release**:
A published, signed version of dokkup that a host can move to: a version tag, a
binary per architecture, its checksum and its signature. Both the version number
and the changelog entry are derived from the commits since the previous Release,
so neither is decided by hand.
_Avoid_: build, tag, artifact

**Update**:
Moving an Installation from the Release it is running to another one. It
replaces the binary and nothing else: not the data directory, not the service,
not the sudoers rule. An Update counts as done only once the new binary answers
its health endpoint reporting its own version; if it does not, the previous
binary is put back.
_Avoid_: upgrade, self-update, patch
