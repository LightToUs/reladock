#!/bin/sh
# Downloaded from the trusted control HTTPS origin; never changes network rules.
set -eu
set +x
umask 077
fail() { echo "$1" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || fail 'Linux is required'
[ "$(id -u)" = 0 ] || fail 'Run on the intended VPS as root (sudo)'
[ "$#" -ge 1 ] && [ "$#" -le 3 ] || fail 'Usage: bootstrap-agent.sh https://control-origin [--check | --offline /absolute/archive]'
origin=$1
bootstrap_check=0
bootstrap_offline=
if [ "$#" -gt 1 ]; then
 case "$2" in --check) [ "$#" = 2 ] || fail 'Unexpected arguments'; bootstrap_check=1;; --offline) [ "$#" = 3 ] || fail 'Offline archive path required'; bootstrap_offline=$3;; *) fail 'Unknown installer option';; esac
fi
case "$origin" in https://*) ;; *) fail 'Trusted HTTPS control origin required';; esac
case "$origin" in *[!a-zA-Z0-9.:/_-]*|*/../*|*//*/*) fail 'Invalid control origin';; esac
authority=${origin#https://}
case "$authority" in ''|*/*) fail 'Origin must not contain a path';; esac
for dependency in curl systemctl sha256sum tar awk sort uniq mktemp install useradd; do
 command -v "$dependency" >/dev/null 2>&1 || fail "Missing dependency: $dependency"
done
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) fail 'Unsupported CPU architecture';; esac
metadata=$(curl --fail --silent --show-error --proto '=https' --connect-timeout 10 --max-time 30 "$origin/api/node-bootstrap/release") || fail 'Cannot fetch pinned release metadata'
set -f
# Metadata has three fixed, independently validated ASCII fields; no eval/source.
set -- $metadata
[ "$#" = 3 ] || fail 'Invalid release metadata'
version=$1
printf '%s\n' "$version" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$/ { ok=1 } END { exit !ok }' || fail 'Invalid pinned release version'
for checksum in "$2" "$3"; do
 [ "${#checksum}" = 64 ] || fail 'Invalid release SHA256'
 case "$checksum" in *[!a-f0-9]*) fail 'Invalid release SHA256';; esac
done
if [ "$arch" = amd64 ]; then digest=$2; else digest=$3; fi
set +f
url="https://github.com/LightToUs/reladock/releases/download/$version/reladock-$version-linux-$arch.tar.gz"
if [ "${bootstrap_check-}" = 1 ]; then echo "$url"; exit 0; fi
[ -d /run/systemd/system ] && [ -d /etc/systemd/system ] || fail 'A running systemd host is required'
[ ! -e /etc/reladock/agent.env ] || fail 'Existing agent.env preserved. Use the documented recovery/upgrade procedure; refusing reinstall.'
[ ! -e /etc/reladock/node-pki ] || fail 'Existing node PKI preserved. Resume enrollment manually; refusing overwrite.'
temp=$(mktemp -d /tmp/reladock-bootstrap.XXXXXX) || exit 1
trap 'rm -rf "$temp"' EXIT HUP INT TERM
if [ -n "${bootstrap_offline-}" ]; then
 case "$bootstrap_offline" in /*) ;; *) fail 'Offline archive must be an absolute path';; esac
 cp "$bootstrap_offline" "$temp/release.tar.gz"
else
 curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 --max-filesize 134217728 "$url" -o "$temp/release.tar.gz" || fail 'GitHub download failed; use the same pinned version offline archive'
fi
printf '%s  %s\n' "$digest" "$temp/release.tar.gz" | sha256sum --check --status || fail 'Release checksum mismatch; nothing installed'
# Archives contain only this allowlist of regular, flat, nonduplicate files.
tar -tzf "$temp/release.tar.gz" > "$temp/names"
awk 'BEGIN {ok=1} !/^(reladock|reladock-admin|reladock-agent|reladock-dataplane|reladock\.service|reladock-agent\.service|reladock-agent-apply\.service|control\.env\.example|agent\.env\.example|install-linux\.sh|bootstrap-agent\.sh|SHA256SUMS|RELEASE\.json|DEPLOYMENT\.md)$/ {ok=0} END {exit !ok}' "$temp/names" || fail 'Unsafe archive paths'
[ -z "$(sort "$temp/names" | uniq -d)" ] || fail 'Duplicate archive entries'
tar -tvzf "$temp/release.tar.gz" | awk 'substr($0,1,1)!="-" {bad=1} END {exit bad}' || fail 'Archive contains non-regular entries'
mkdir "$temp/release"
tar -xzf "$temp/release.tar.gz" -C "$temp/release"
(cd "$temp/release" && sha256sum --check SHA256SUMS) || fail 'Package file validation failed'
# All binaries/templates are verified before execution.
install -d -o root -g root -m 0755 /opt/reladock
if ! id reladock >/dev/null 2>&1; then useradd --system --user-group --home-dir /var/lib/reladock --shell /usr/sbin/nologin reladock; fi
install -d -o root -g reladock -m 0750 /etc/reladock
install -o root -g root -m 0755 "$temp/release/reladock-agent" /opt/reladock/reladock-agent
install -o root -g root -m 0755 "$temp/release/reladock-dataplane" /opt/reladock/reladock-dataplane
# Token travels over stdin, never argv, environment, URL or a persistent file.
/opt/reladock/reladock-agent enroll --bootstrap-origin "$origin" --pki /etc/reladock/node-pki --env-file /etc/reladock/agent.env || fail 'Enrollment failed. Private enrollment state retained; see recovery instructions.'
chown reladock:reladock /etc/reladock/node-pki /etc/reladock/node-pki/*
chmod 0700 /etc/reladock/node-pki
chmod 0600 /etc/reladock/node-pki/* /etc/reladock/agent.env
install -o root -g root -m 0644 "$temp/release/reladock-agent.service" /etc/systemd/system/reladock-agent.service
install -o root -g root -m 0644 "$temp/release/reladock-agent-apply.service" /etc/systemd/system/reladock-agent-apply.service
systemctl daemon-reload
systemctl enable --now reladock-agent
systemctl is-active --quiet reladock-agent || fail 'Agent failed to start. Enrollment retained; inspect journalctl -u reladock-agent'
echo 'Agent started in validation-only mode. Check contact and configuration results in the console. Forwarding is NOT enabled.'
