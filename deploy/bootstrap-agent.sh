#!/bin/sh
# Downloaded from the trusted control HTTPS origin; never changes network rules.
set -eu
set +x
umask 077
fail() { echo "$1" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || fail 'Linux is required'
[ "$(id -u)" = 0 ] || fail 'Run on the intended VPS as root (sudo)'
[ "$#" -ge 1 ] || fail 'Usage: bootstrap-agent.sh https://control-origin [--check | --offline /absolute/archive] [--existing-action upgrade|none]'
origin=$1
shift
bootstrap_check=0
bootstrap_offline=
bootstrap_existing_action=
while [ "$#" -gt 0 ]; do
 case "$1" in
  --check) [ "$bootstrap_check" = 0 ] && [ -z "$bootstrap_offline" ] || fail 'Duplicate or conflicting installer option'; bootstrap_check=1; shift;;
  --offline) [ "$bootstrap_check" = 0 ] && [ -z "$bootstrap_offline" ] && [ "$#" -ge 2 ] || fail 'Offline archive path required'; bootstrap_offline=$2; shift 2;;
  --existing-action) [ -z "$bootstrap_existing_action" ] && [ "$#" -ge 2 ] || fail 'Existing installation action required'; bootstrap_existing_action=$2; shift 2;;
  *) fail 'Unknown installer option';;
 esac
done
case "$bootstrap_existing_action" in ''|upgrade|none) ;; *) fail 'Existing installation action must be upgrade or none';; esac
[ "$bootstrap_check" = 0 ] || [ -z "$bootstrap_existing_action" ] || fail 'Existing installation action is not valid with --check'
case "$origin" in https://*) ;; *) fail 'Trusted HTTPS control origin required';; esac
case "$origin" in *[!a-zA-Z0-9.:/_-]*|*/../*|*//*/*) fail 'Invalid control origin';; esac
authority=${origin#https://}
case "$authority" in ''|*/*) fail 'Origin must not contain a path';; esac
for dependency in curl systemctl sha256sum tar awk sort uniq mktemp install cp mv cat useradd; do
 command -v "$dependency" >/dev/null 2>&1 || fail "Missing dependency: $dependency"
done
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) fail 'Unsupported CPU architecture';; esac
bootstrap_upgrade=0
if [ "$bootstrap_check" = 0 ]; then
 [ -d /run/systemd/system ] && [ -d /etc/systemd/system ] || fail 'A running systemd host is required'
 existing_observed=0
 for existing_path in /etc/reladock/agent.env /etc/reladock/node-pki /opt/reladock/reladock-agent /opt/reladock/reladock-dataplane /opt/reladock/reladock-updater /opt/reladock/reladock-recovery /etc/systemd/system/reladock-agent.service /etc/systemd/system/reladock-agent-apply.service /etc/systemd/system/reladock-updater.service /etc/systemd/system/reladock-updater.timer; do
  if [ -e "$existing_path" ] || [ -L "$existing_path" ]; then existing_observed=1; fi
 done
 if [ "$existing_observed" = 1 ]; then
 existing_complete=1
 [ -f /etc/reladock/agent.env ] && [ ! -L /etc/reladock/agent.env ] || existing_complete=0
 [ -d /etc/reladock/node-pki ] && [ ! -L /etc/reladock/node-pki ] || existing_complete=0
 for path in agent.key enrollment.json agent.crt ca.crt control node-id; do
  [ -f "/etc/reladock/node-pki/$path" ] && [ ! -L "/etc/reladock/node-pki/$path" ] || existing_complete=0
 done
 if [ -e /etc/reladock/node-pki/identity.pem ] || [ -L /etc/reladock/node-pki/identity.pem ]; then
  [ -f /etc/reladock/node-pki/identity.pem ] && [ ! -L /etc/reladock/node-pki/identity.pem ] || existing_complete=0
 fi
 for path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
  [ -f "/opt/reladock/$path" ] && [ ! -L "/opt/reladock/$path" ] || existing_complete=0
 done
 for path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
  [ -f "/etc/systemd/system/$path" ] && [ ! -L "/etc/systemd/system/$path" ] || existing_complete=0
 done
 if [ "$existing_complete" = 1 ]; then
  env_node=$(awk '$0 ~ /^RELADOCK_NODE=/ {sub(/^RELADOCK_NODE=/, ""); print}' /etc/reladock/agent.env)
  env_control=$(awk '$0 ~ /^RELADOCK_CONTROL=/ {sub(/^RELADOCK_CONTROL=/, ""); print}' /etc/reladock/agent.env)
  env_pki=$(awk '$0 ~ /^RELADOCK_PKI=/ {sub(/^RELADOCK_PKI=/, ""); print}' /etc/reladock/agent.env)
  [ "$env_node" = "$(cat /etc/reladock/node-pki/node-id)" ] || existing_complete=0
  [ "$env_control" = "$(cat /etc/reladock/node-pki/control)" ] || existing_complete=0
  [ "$env_pki" = /etc/reladock/node-pki ] || existing_complete=0
 fi
 [ "$existing_complete" = 1 ] || fail 'Existing Agent state is incomplete or conflicting. Preserve agent.env, PKI, WAL and journal; use the documented recovery procedure.'
 case "$bootstrap_existing_action" in
  none) echo 'Existing complete Agent installation left unchanged. The new bootstrap token was not consumed; revoke it in the console.'; exit 0;;
  upgrade) bootstrap_upgrade=1;;
  '')
   if (exec 3<>/dev/tty) 2>/dev/null; then
    exec 3<>/dev/tty
    printf '%s\n' 'Existing complete Agent installation detected.' >&3
    printf '%s\n' '1) Upgrade Agent from the pinned release' >&3
    printf '%s\n' '2) No action (default)' >&3
    printf '%s' 'Select [1/2]: ' >&3
    IFS= read -r bootstrap_selection <&3 || bootstrap_selection=
    exec 3>&-
    case "$bootstrap_selection" in
     1) bootstrap_upgrade=1;;
     *) echo 'Existing complete Agent installation left unchanged. The new bootstrap token was not consumed; revoke it in the console.'; exit 0;;
    esac
   else
    echo 'Existing complete Agent installation left unchanged because no interactive terminal is available. The new bootstrap token was not consumed; revoke it in the console.'
    exit 0
   fi
   ;;
 esac
 elif [ -n "$bootstrap_existing_action" ]; then
  fail 'No existing Agent installation was found for the requested action.'
 fi
fi
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
if [ "$bootstrap_check" = 1 ]; then echo "$url"; exit 0; fi
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
awk 'BEGIN {ok=1} !/^(reladock|reladock-admin|reladock-agent|reladock-dataplane|reladock-updater|reladock-recovery|reladock\.service|reladock-agent\.service|reladock-agent-apply\.service|reladock-updater\.service|reladock-updater\.timer|control\.env\.example|agent\.env\.example|install-linux\.sh|bootstrap-agent\.sh|SHA256SUMS|RELEASE\.json|DEPLOYMENT\.md|LINK-AND-CAPACITY\.md|OFFSITE-RECOVERY\.md|SAFE-RETIREMENT\.md)$/ {ok=0} {count++} END {exit !ok || count!=21}' "$temp/names" || fail 'Incomplete or unsafe archive paths'
[ -z "$(sort "$temp/names" | uniq -d)" ] || fail 'Duplicate archive entries'
tar -tvzf "$temp/release.tar.gz" | awk 'substr($0,1,1)!="-" {bad=1} END {exit bad}' || fail 'Archive contains non-regular entries'
mkdir "$temp/release"
tar -xzf "$temp/release.tar.gz" -C "$temp/release"
(cd "$temp/release" && sha256sum --check SHA256SUMS) || fail 'Package file validation failed'
# All binaries/templates are verified before execution.
if [ "$bootstrap_upgrade" = 1 ]; then
 validation_active=0
 apply_active=0
 validation_enabled=0
 apply_enabled=0
 updater_timer_active=0
 systemctl is-active --quiet reladock-agent.service && validation_active=1
 systemctl is-active --quiet reladock-agent-apply.service && apply_active=1
 systemctl is-enabled --quiet reladock-agent.service && validation_enabled=1
 systemctl is-enabled --quiet reladock-agent-apply.service && apply_enabled=1
 systemctl is-active --quiet reladock-updater.timer && updater_timer_active=1
 [ "$validation_active" = 0 ] || [ "$apply_active" = 0 ] || fail 'Both Agent modes are active; refusing ambiguous upgrade.'
 upgrade_mode=validation
 upgrade_unit=reladock-agent.service
 if [ "$apply_active" = 1 ]; then upgrade_mode=apply; upgrade_unit=reladock-agent-apply.service; fi
 systemctl is-active --quiet reladock-updater.service && fail 'A controlled Agent upgrade is already running.'
 upgrade_backup="$temp/upgrade-backup"
 mkdir -p "$upgrade_backup/opt" "$upgrade_backup/systemd"
 for path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
  cp -p "/opt/reladock/$path" "$upgrade_backup/opt/$path" || fail 'Cannot back up the existing Agent binaries.'
 done
 for path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
  cp -p "/etc/systemd/system/$path" "$upgrade_backup/systemd/$path" || fail 'Cannot back up the existing Agent service units.'
 done
 rollback_upgrade() {
  rollback_ok=1
  systemctl stop reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer >/dev/null 2>&1 || rollback_ok=0
  for rollback_path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
   rm -f "/opt/reladock/$rollback_path.next" || rollback_ok=0
   cp -p "$upgrade_backup/opt/$rollback_path" "/opt/reladock/$rollback_path" || rollback_ok=0
  done
  for rollback_path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
   cp -p "$upgrade_backup/systemd/$rollback_path" "/etc/systemd/system/$rollback_path" || rollback_ok=0
  done
  systemctl daemon-reload || rollback_ok=0
  if [ "$validation_enabled" = 1 ]; then systemctl enable reladock-agent.service || rollback_ok=0; else systemctl disable reladock-agent.service || rollback_ok=0; fi
  if [ "$apply_enabled" = 1 ]; then systemctl enable reladock-agent-apply.service || rollback_ok=0; else systemctl disable reladock-agent-apply.service || rollback_ok=0; fi
  [ "$validation_active" = 0 ] || systemctl start reladock-agent.service || rollback_ok=0
  [ "$apply_active" = 0 ] || systemctl start reladock-agent-apply.service || rollback_ok=0
  [ "$updater_timer_active" = 0 ] || systemctl start reladock-updater.timer || rollback_ok=0
  [ "$validation_active" = 0 ] || systemctl is-active --quiet reladock-agent.service || rollback_ok=0
  [ "$apply_active" = 0 ] || systemctl is-active --quiet reladock-agent-apply.service || rollback_ok=0
  [ "$updater_timer_active" = 0 ] || systemctl is-active --quiet reladock-updater.timer || rollback_ok=0
  [ "$rollback_ok" = 1 ]
 }
 upgrade_failed() {
  upgrade_reason=$1
  if rollback_upgrade; then
   echo "$upgrade_reason Previous binaries, service units and prior service mode were restored." >&2
  else
   echo "$upgrade_reason Automatic rollback was incomplete; keep all PKI, WAL and journal state and inspect systemd before any retry." >&2
  fi
  exit 1
 }
 install_upgrade_release() {
  for install_path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
   install -o root -g root -m 0755 "$temp/release/$install_path" "/opt/reladock/$install_path.next" || return 1
   mv -f "/opt/reladock/$install_path.next" "/opt/reladock/$install_path" || return 1
  done
  for install_path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
   install -o root -g root -m 0644 "$temp/release/$install_path" "/etc/systemd/system/$install_path" || return 1
  done
  systemctl daemon-reload
 }
 if [ "$updater_timer_active" = 1 ] && ! systemctl stop reladock-updater.timer; then upgrade_failed 'Cannot stop the updater timer for upgrade.'; fi
 if [ "$validation_active" = 1 ] && ! systemctl stop reladock-agent.service; then upgrade_failed 'Cannot stop the validation Agent for upgrade.'; fi
 if [ "$apply_active" = 1 ] && ! systemctl stop reladock-agent-apply.service; then upgrade_failed 'Cannot stop the apply Agent for upgrade.'; fi
 if ! install_upgrade_release; then upgrade_failed 'Pinned release installation failed.'; fi
 if [ "$upgrade_mode" = validation ]; then
  if ! systemctl disable reladock-agent-apply.service; then upgrade_failed 'Cannot disable the inactive apply Agent for safe reboot.'; fi
  if ! systemctl enable --now reladock-agent.service; then upgrade_failed 'Upgraded validation Agent failed to enable and start.'; fi
 else
  if ! systemctl disable reladock-agent.service; then upgrade_failed 'Cannot disable the inactive validation Agent for safe reboot.'; fi
  if ! systemctl enable --now reladock-agent-apply.service; then upgrade_failed 'Upgraded apply Agent failed to enable and start.'; fi
 fi
 if [ "$updater_timer_active" = 1 ] && ! systemctl start reladock-updater.timer; then upgrade_failed 'Updater timer failed to resume after upgrade.'; fi
 if ! systemctl is-active --quiet "$upgrade_unit"; then
  if [ "$upgrade_mode" = validation ]; then upgrade_failed 'Upgraded validation Agent is not active.'; else upgrade_failed 'Upgraded apply Agent is not active.'; fi
 fi
 if ! systemctl is-enabled --quiet "$upgrade_unit"; then
  if [ "$upgrade_mode" = validation ]; then upgrade_failed 'Upgraded validation Agent is not enabled for reboot.'; else upgrade_failed 'Upgraded apply Agent is not enabled for reboot.'; fi
 fi
 if [ "$updater_timer_active" = 1 ] && ! systemctl is-active --quiet reladock-updater.timer; then upgrade_failed 'Updater timer is not active after upgrade.'; fi
 echo "Existing Agent upgraded from the pinned release. Identity, environment, WAL and journal were preserved. The $upgrade_mode Agent is active and enabled for reboot."
 echo 'The new bootstrap token was not consumed; revoke it in the console. Service activity is not end-to-end forwarding health.'
 exit 0
fi
install -d -o root -g root -m 0755 /opt/reladock
if ! id reladock >/dev/null 2>&1; then useradd --system --user-group --home-dir /var/lib/reladock --shell /usr/sbin/nologin reladock; fi
install -d -o root -g reladock -m 0750 /etc/reladock
install -d -o reladock -g reladock -m 0700 /var/lib/reladock-agent/control
install -d -o root -g reladock -m 0750 /var/lib/reladock-locks
install -o root -g reladock -m 0640 /dev/null /var/lib/reladock-locks/control.lock
install -o root -g root -m 0755 "$temp/release/reladock-agent" /opt/reladock/reladock-agent
install -o root -g root -m 0755 "$temp/release/reladock-dataplane" /opt/reladock/reladock-dataplane
install -o root -g root -m 0755 "$temp/release/reladock-updater" /opt/reladock/reladock-updater
install -o root -g root -m 0755 "$temp/release/reladock-recovery" /opt/reladock/reladock-recovery
# Token travels over stdin, never argv, environment, URL or a persistent file.
/opt/reladock/reladock-agent enroll --bootstrap-origin "$origin" --pki /etc/reladock/node-pki --env-file /etc/reladock/agent.env || fail 'Enrollment failed. Private enrollment state retained; see recovery instructions.'
chown reladock:reladock /etc/reladock/node-pki /etc/reladock/node-pki/*
chmod 0700 /etc/reladock/node-pki
chmod 0600 /etc/reladock/node-pki/* /etc/reladock/agent.env
install -o root -g root -m 0644 "$temp/release/reladock-agent.service" /etc/systemd/system/reladock-agent.service
install -o root -g root -m 0644 "$temp/release/reladock-agent-apply.service" /etc/systemd/system/reladock-agent-apply.service
install -o root -g root -m 0644 "$temp/release/reladock-updater.service" /etc/systemd/system/reladock-updater.service
install -o root -g root -m 0644 "$temp/release/reladock-updater.timer" /etc/systemd/system/reladock-updater.timer
systemctl daemon-reload
systemctl enable --now reladock-agent
systemctl is-active --quiet reladock-agent || fail 'Agent failed to start. Enrollment retained; inspect journalctl -u reladock-agent'
systemctl enable --now reladock-updater.timer
systemctl is-active --quiet reladock-updater.timer || fail 'Updater timer failed to start. Agent remains installed; inspect journalctl -u reladock-updater.timer'
echo 'Agent started in validation-only mode. Check contact and configuration results in the console. Forwarding is NOT enabled.'
