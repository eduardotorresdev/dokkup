# Development environment for dokkup: a real Dokku host in a container.
#
# systemd runs as PID 1, Docker runs under systemd, and Dokku is installed by the
# upstream bootstrap script on first boot. This is what lets us exercise
# `dokkup install` for real -- it writes a systemd unit, a sudoers rule and a
# system user, none of which can be tested against Dokku's official image, which
# has no init system.
#
# Builds with `container build` on macOS (Apple Silicon) and `docker build` on
# Linux CI. Requires --cap-add ALL at run time; privileged is neither used nor
# needed. See docs/adr/0011 for what was verified and what does not work.
#
# Dokku is installed on first boot rather than during the build because the
# bootstrap script needs a running Docker daemon, and a build step cannot have
# one without build-time privileges that Apple's runtime does not offer. The
# first `make devenv-up` therefore takes several minutes; later ones do not,
# because the container is kept rather than recreated.

FROM ubuntu:26.04

ARG DOKKU_VERSION=v0.38.7
ARG DOKKU_HOSTNAME=dokku.me

ENV DEBIAN_FRONTEND=noninteractive \
    container=docker

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus \
      docker.io \
      openssh-server \
      ca-certificates curl wget gnupg sudo \
      iproute2 iptables netcat-openbsd procps \
      git make jq \
 && rm -rf /var/lib/apt/lists/*

# Strip the units a container has no use for. Keeping them only produces failed
# units, which makes `systemctl --failed` useless for spotting real problems.
RUN rm -f /lib/systemd/system/multi-user.target.wants/* \
          /etc/systemd/system/*.wants/* \
          /lib/systemd/system/local-fs.target.wants/* \
          /lib/systemd/system/sockets.target.wants/*udev* \
          /lib/systemd/system/sockets.target.wants/*initctl* \
          /lib/systemd/system/basic.target.wants/* \
 && systemctl set-default multi-user.target

# sshd is what makes `git push dokku main` work, so the dev environment needs it
# enabled explicitly -- the unit is among those stripped above.
RUN mkdir -p /run/sshd \
 && ssh-keygen -A \
 && systemctl enable ssh docker

COPY bootstrap-dokku.sh /usr/local/sbin/bootstrap-dokku.sh
COPY dokku-bootstrap.service /etc/systemd/system/dokku-bootstrap.service

RUN chmod 0755 /usr/local/sbin/bootstrap-dokku.sh \
 && printf 'DOKKU_TAG=%s\nDOKKU_HOSTNAME=%s\n' "${DOKKU_VERSION}" "${DOKKU_HOSTNAME}" \
      > /etc/dokku-bootstrap.env \
 && systemctl enable dokku-bootstrap.service

EXPOSE 22 80 443 8080

STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
