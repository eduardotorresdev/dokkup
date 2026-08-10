# The development environment runs a real Dokku inside Apple's container runtime

Development on macOS uses Apple's `container` runtime to run one Ubuntu container
with systemd as PID 1, Docker underneath it, and a real Dokku installed by the
upstream bootstrap script. The same image definition builds with `docker build` on
Linux CI runners.

This is worth recording because the obvious expectation is that it cannot work.
Dokku's own documentation runs it in a container by mounting the host's Docker
socket, and Apple's runtime is not Docker and exposes no such socket. The reason
it works anyway is that Apple's runtime gives each container its own lightweight
Linux virtual machine, and that kernel ships the pieces Docker needs.

Verified on Apple container 1.0.0, macOS 26.5, Apple Silicon:

- kernel 6.18.15 with `NETFILTER`, `NF_NAT`, `IP_NF_IPTABLES`, `BRIDGE`, `VETH`
  and `OVERLAY_FS` built in, and cgroup v2 available
- systemd as PID 1 reaches a running state with no failed units
- dockerd starts under systemd and runs nested containers on `overlay2`
- Dokku 0.38.7 installs to completion, with nginx up and `apps:create`,
  `config:set` and the JSON reports all working

Only `--cap-add ALL` is required; there is no privileged flag and none is needed.

Under Docker the same image needs more, because Docker shares the host kernel
instead of booting one: `--privileged`, `--cgroupns=host`, a writable
`/sys/fs/cgroup` and tmpfs for `/run`. The Makefile chooses the flags from the
runtime it finds, so the difference is not something anyone has to remember.

## Consequences

The installer is exercised for real. It writes a systemd unit, a sudoers rule and
a system user, none of which can be tested against Dokku's official container
image, which has no init system.

Container resource limits do not apply — dockerd reports a non-fatal failure
writing cgroup subtree controllers. Nothing dokkup does depends on them, but the
development environment is not the place to observe an App being throttled.
