#!/bin/sh
# Run explicitly on the target Linux host after reviewing DEPLOYMENT.md.
set -eu
umask 077
if [ "$(uname -s)" != Linux ]; then echo 'Linux is required' >&2; exit 1; fi
if [ "$(id -u)" != 0 ]; then echo 'Run as root on the intended server' >&2; exit 1; fi
if [ "$#" != 2 ]; then echo 'Usage: sh install-linux.sh control|agent /absolute/release-directory' >&2; exit 1; fi
component=$1
release=$2
case "$component" in control) service=reladock; configuration=control; binaries='reladock reladock-admin';; agent) service=reladock-agent; configuration=agent; binaries='reladock-agent reladock-dataplane';; *) exit 1;; esac
case "$release" in /*) ;; *) echo 'Release directory must be absolute' >&2; exit 1;; esac
command -v systemctl >/dev/null
command -v sha256sum >/dev/null
test -f "$release/SHA256SUMS"
(cd "$release" && sha256sum --check SHA256SUMS)
for binary in $binaries; do test -f "$release/$binary"; done
test -f "$release/$service.service"
test -f "$release/$configuration.env.example"
if ! id reladock >/dev/null 2>&1; then useradd --system --user-group --home-dir /var/lib/reladock --shell /usr/sbin/nologin reladock; fi
install -d -o root -g root -m 0755 /opt/reladock
install -d -o root -g reladock -m 0750 /etc/reladock
install -d -o reladock -g reladock -m 0700 /var/lib/reladock
if [ ! -f "/etc/reladock/$configuration.env" ]; then
  install -o root -g root -m 0600 "$release/$configuration.env.example" "/etc/reladock/$configuration.env"
fi
for binary in $binaries; do
  install -o root -g root -m 0755 "$release/$binary" "/opt/reladock/$binary.next"
  mv -f "/opt/reladock/$binary.next" "/opt/reladock/$binary"
done
install -o root -g root -m 0644 "$release/$service.service" "/etc/systemd/system/$service.service"
if [ "$component" = agent ]; then
  test -f "$release/reladock-agent-apply.service"
  install -o root -g root -m 0644 "$release/reladock-agent-apply.service" /etc/systemd/system/reladock-agent-apply.service
fi
systemctl daemon-reload
echo "Installed $service. Review /etc/reladock/$configuration.env and certificates."
echo 'Existing environment files were preserved. No service has been started or enabled.'
echo "After configuration and acceptance checks: systemctl enable --now $service"
if [ "$component" = agent ]; then echo 'Application is a separate explicit unit; review the shaping lease, offload and network prerequisites before selecting it.'; fi
