#!/bin/sh
# Downloaded from the trusted control HTTPS origin; changes network prerequisites
# only after an interactive, explicit apply-mode confirmation.
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
 for existing_path in /etc/reladock/agent.env /etc/reladock/agent.env.pre-apply /etc/reladock/shaping.json /etc/reladock/network-managed /etc/reladock/node-pki /opt/reladock/reladock-agent /opt/reladock/reladock-dataplane /opt/reladock/reladock-updater /opt/reladock/reladock-recovery /etc/systemd/system/reladock-agent.service /etc/systemd/system/reladock-agent-apply.service /etc/systemd/system/reladock-agent-network.service /etc/systemd/system/reladock-updater.service /etc/systemd/system/reladock-updater.timer; do
  if [ -e "$existing_path" ] || [ -L "$existing_path" ]; then existing_observed=1; fi
 done
 if [ "$existing_observed" = 1 ]; then
 existing_complete=1
 [ -f /etc/reladock/agent.env ] && [ ! -L /etc/reladock/agent.env ] || existing_complete=0
 [ -d /etc/reladock/node-pki ] && [ ! -L /etc/reladock/node-pki ] || existing_complete=0
 for path in agent.key enrollment.json agent.crt ca.crt identity.pem control node-id; do
  [ -f "/etc/reladock/node-pki/$path" ] && [ ! -L "/etc/reladock/node-pki/$path" ] || existing_complete=0
 done
 for path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
  [ -f "/opt/reladock/$path" ] && [ ! -L "/opt/reladock/$path" ] || existing_complete=0
 done
 for path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
  [ -f "/etc/systemd/system/$path" ] && [ ! -L "/etc/systemd/system/$path" ] || existing_complete=0
 done
 if [ -e /etc/systemd/system/reladock-agent-network.service ] || [ -L /etc/systemd/system/reladock-agent-network.service ]; then
  [ -f /etc/systemd/system/reladock-agent-network.service ] && [ ! -L /etc/systemd/system/reladock-agent-network.service ] || existing_complete=0
 fi
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
apply_preparation_active=0
apply_preserve_temp=0
bootstrap_cleanup() {
 if [ "$apply_preparation_active" = 1 ]; then
  if rollback_apply_preparation; then
   apply_preparation_active=0
  else
   apply_preparation_active=0
   apply_preserve_temp=1
   echo "Interrupted apply preparation could not be fully rolled back. Preserve /etc/reladock/agent.env.pre-apply and $temp for recovery." >&2
  fi
 fi
 [ "$apply_preserve_temp" = 1 ] || rm -rf "$temp"
}
trap 'exit 1' HUP INT TERM
trap 'bootstrap_cleanup' EXIT
if [ -n "${bootstrap_offline-}" ]; then
 case "$bootstrap_offline" in /*) ;; *) fail 'Offline archive must be an absolute path';; esac
 cp "$bootstrap_offline" "$temp/release.tar.gz"
else
 curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 --max-filesize 134217728 "$url" -o "$temp/release.tar.gz" || fail 'GitHub download failed; use the same pinned version offline archive'
fi
printf '%s  %s\n' "$digest" "$temp/release.tar.gz" | sha256sum --check --status || fail 'Release checksum mismatch; nothing installed'
# Archives contain only this allowlist of regular, flat, nonduplicate files.
tar -tzf "$temp/release.tar.gz" > "$temp/names"
awk 'BEGIN {ok=1} !/^(reladock|reladock-admin|reladock-agent|reladock-dataplane|reladock-updater|reladock-recovery|reladock\.service|reladock-agent\.service|reladock-agent-apply\.service|reladock-agent-network\.service|reladock-updater\.service|reladock-updater\.timer|control\.env\.example|agent\.env\.example|install-linux\.sh|bootstrap-agent\.sh|SHA256SUMS|RELEASE\.json|DEPLOYMENT\.md|LINK-AND-CAPACITY\.md|OFFSITE-RECOVERY\.md|SAFE-RETIREMENT\.md)$/ {ok=0} {count++} END {exit !ok || count!=22}' "$temp/names" || fail 'Incomplete or unsafe archive paths'
[ -z "$(sort "$temp/names" | uniq -d)" ] || fail 'Duplicate archive entries'
tar -tvzf "$temp/release.tar.gz" | awk 'substr($0,1,1)!="-" {bad=1} END {exit bad}' || fail 'Archive contains non-regular entries'
mkdir "$temp/release"
tar -xzf "$temp/release.tar.gz" -C "$temp/release"
(cd "$temp/release" && sha256sum --check SHA256SUMS) || fail 'Package file validation failed'
# All binaries/templates are verified before execution.
if [ "$bootstrap_upgrade" = 1 ]; then
 validation_active=0
 apply_active=0
 updater_timer_active=0
 systemctl is-active --quiet reladock-agent.service && validation_active=1
 systemctl is-active --quiet reladock-agent-apply.service && apply_active=1
 systemctl is-active --quiet reladock-updater.timer && updater_timer_active=1
 [ "$validation_active" = 0 ] || [ "$apply_active" = 0 ] || fail 'Both Agent modes are active; refusing ambiguous upgrade.'
 systemctl is-active --quiet reladock-updater.service && fail 'A controlled Agent upgrade is already running.'
 upgrade_backup="$temp/upgrade-backup"
 mkdir -p "$upgrade_backup/opt" "$upgrade_backup/systemd"
 for path in reladock-agent reladock-dataplane reladock-updater reladock-recovery; do
  cp -p "/opt/reladock/$path" "$upgrade_backup/opt/$path" || fail 'Cannot back up the existing Agent binaries.'
 done
 for path in reladock-agent.service reladock-agent-apply.service reladock-updater.service reladock-updater.timer; do
  cp -p "/etc/systemd/system/$path" "$upgrade_backup/systemd/$path" || fail 'Cannot back up the existing Agent service units.'
 done
 if [ -f /etc/systemd/system/reladock-agent-network.service ] && [ ! -L /etc/systemd/system/reladock-agent-network.service ]; then
  cp -p /etc/systemd/system/reladock-agent-network.service "$upgrade_backup/systemd/reladock-agent-network.service" || fail 'Cannot back up the existing Agent network service unit.'
 fi
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
  if [ -f "$upgrade_backup/systemd/reladock-agent-network.service" ]; then
   cp -p "$upgrade_backup/systemd/reladock-agent-network.service" /etc/systemd/system/reladock-agent-network.service || rollback_ok=0
  else
   rm -f /etc/systemd/system/reladock-agent-network.service || rollback_ok=0
  fi
  systemctl daemon-reload || rollback_ok=0
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
  for install_path in reladock-agent.service reladock-agent-apply.service reladock-agent-network.service reladock-updater.service reladock-updater.timer; do
   install -o root -g root -m 0644 "$temp/release/$install_path" "/etc/systemd/system/$install_path" || return 1
  done
  systemctl daemon-reload
 }
 if [ "$updater_timer_active" = 1 ] && ! systemctl stop reladock-updater.timer; then upgrade_failed 'Cannot stop the updater timer for upgrade.'; fi
 if [ "$validation_active" = 1 ] && ! systemctl stop reladock-agent.service; then upgrade_failed 'Cannot stop the validation Agent for upgrade.'; fi
 if [ "$apply_active" = 1 ] && ! systemctl stop reladock-agent-apply.service; then upgrade_failed 'Cannot stop the apply Agent for upgrade.'; fi
 if ! install_upgrade_release; then upgrade_failed 'Pinned release installation failed.'; fi
 if [ "$validation_active" = 1 ] && ! systemctl start reladock-agent.service; then upgrade_failed 'Upgraded validation Agent failed to start.'; fi
 if [ "$apply_active" = 1 ] && ! systemctl start reladock-agent-apply.service; then upgrade_failed 'Upgraded apply Agent failed to start.'; fi
 if [ "$updater_timer_active" = 1 ] && ! systemctl start reladock-updater.timer; then upgrade_failed 'Updater timer failed to resume after upgrade.'; fi
 if [ "$validation_active" = 1 ] && ! systemctl is-active --quiet reladock-agent.service; then upgrade_failed 'Upgraded validation Agent is not active.'; fi
 if [ "$apply_active" = 1 ] && ! systemctl is-active --quiet reladock-agent-apply.service; then upgrade_failed 'Upgraded apply Agent is not active.'; fi
 if [ "$updater_timer_active" = 1 ] && ! systemctl is-active --quiet reladock-updater.timer; then upgrade_failed 'Updater timer is not active after upgrade.'; fi
 echo 'Existing Agent upgraded from the pinned release. Identity, environment, WAL and journal were preserved. The prior service mode was restored.'
 echo 'The new bootstrap token was not consumed; revoke it in the console. Service activity is not end-to-end forwarding health.'
 exit 0
fi

rollback_apply_preparation() {
 rollback_ok=1
 systemctl disable --now reladock-agent-network.service >/dev/null 2>&1 || rollback_ok=0
 rm -f /etc/reladock/network-managed /etc/reladock/shaping.json || rollback_ok=0
 cp -p /etc/reladock/agent.env.pre-apply /etc/reladock/agent.env || rollback_ok=0
 /usr/sbin/sysctl -w "net.ipv4.ip_forward=$apply_old_ip_forward" >/dev/null 2>&1 || rollback_ok=0
 /usr/sbin/ethtool -K "$apply_interface" gro "$apply_old_gro" gso "$apply_old_gso" tso "$apply_old_tso" lro "$apply_old_lro" >/dev/null 2>&1 || rollback_ok=0
 /usr/sbin/ip link set dev "$apply_interface" alias "$apply_old_alias" >/dev/null 2>&1 || rollback_ok=0
 systemctl enable --now reladock-agent.service >/dev/null 2>&1 || rollback_ok=0
 if [ "$rollback_ok" = 1 ]; then
  rm -f /etc/reladock/agent.env.pre-apply || rollback_ok=0
 fi
 [ "$rollback_ok" = 1 ]
}

prepare_fresh_apply() {
 for apply_dependency in /usr/sbin/ip /usr/sbin/tc /usr/sbin/nft /usr/sbin/ethtool /usr/sbin/conntrack /usr/sbin/sysctl; do
  [ -x "$apply_dependency" ] || { echo "Apply mode unavailable: missing dependency $apply_dependency; validation-only remains active." >&2; return 1; }
 done
 apply_routes=$(/usr/sbin/ip -4 -o route show default) || { echo 'Apply mode unavailable: cannot inspect the IPv4 default route.' >&2; return 1; }
 apply_route_count=$(printf '%s\n' "$apply_routes" | awk 'NF {count++} END {print count+0}')
 [ "$apply_route_count" = 1 ] || { echo 'Apply mode unavailable: exactly one IPv4 default route is required.' >&2; return 1; }
 apply_interface=$(printf '%s\n' "$apply_routes" | awk 'NR==1 {for(i=1;i<=NF;i++) if($i=="dev" && i<NF) {print $(i+1); exit}}')
 case "$apply_interface" in ''|lo|*[!a-zA-Z0-9_.:-]*|????????????????*) echo 'Apply mode unavailable: invalid or unsupported default-route interface.' >&2; return 1;; esac
 apply_addresses=$(/usr/sbin/ip -4 -o address show dev "$apply_interface" scope global | awk '{split($4,a,"/"); if(a[1] != "") print a[1]}') || { echo 'Apply mode unavailable: cannot inspect the interface address.' >&2; return 1; }
 apply_address_count=$(printf '%s\n' "$apply_addresses" | awk 'NF {count++} END {print count+0}')
 [ "$apply_address_count" = 1 ] || { echo 'Apply mode unavailable: exactly one locally assigned global IPv4 address is required.' >&2; return 1; }
 apply_snat=$(printf '%s\n' "$apply_addresses" | awk 'NF {print; exit}')
 printf '%s\n' "$apply_snat" | awk -F. 'NF!=4 {exit 1} {for(i=1;i<=4;i++) if($i !~ /^[0-9]+$/ || $i<0 || $i>255) exit 1}' || { echo 'Apply mode unavailable: invalid IPv4 address.' >&2; return 1; }
 apply_authoritative_listen=$(cat /etc/reladock/node-pki/listen-address 2>/dev/null) || { echo 'Apply mode unavailable: enrolled listen address is unavailable.' >&2; return 1; }
 [ "$apply_snat" = "$apply_authoritative_listen" ] || { echo 'Apply mode unavailable: the control-plane listen address is not assigned to the reviewed interface.' >&2; return 1; }
 apply_ifindex=$(cat "/sys/class/net/$apply_interface/ifindex" 2>/dev/null) || { echo 'Apply mode unavailable: interface index is unavailable.' >&2; return 1; }
 case "$apply_ifindex" in ''|*[!0-9]*) echo 'Apply mode unavailable: invalid interface index.' >&2; return 1;; esac
 apply_namespace=$(readlink /proc/self/ns/net 2>/dev/null) || { echo 'Apply mode unavailable: network namespace identity is unavailable.' >&2; return 1; }
 apply_node=$(cat /etc/reladock/node-pki/node-id)
 apply_expected_alias="reladock-shaping-v1:$apply_node"
 apply_old_alias=$(cat "/sys/class/net/$apply_interface/ifalias" 2>/dev/null) || { echo 'Apply mode unavailable: interface alias is unavailable.' >&2; return 1; }
 [ -z "$apply_old_alias" ] || [ "$apply_old_alias" = "$apply_expected_alias" ] || { echo 'Apply mode unavailable: the interface alias is owned by another operator.' >&2; return 1; }
 apply_classes=$(/usr/sbin/tc -j class show dev "$apply_interface") || { echo 'Apply mode unavailable: tc class inspection failed.' >&2; return 1; }
 apply_filters=$(/usr/sbin/tc -j filter show dev "$apply_interface") || { echo 'Apply mode unavailable: tc filter inspection failed.' >&2; return 1; }
 [ "$(printf '%s' "$apply_classes" | awk '{gsub(/[[:space:]]/,"");printf "%s",$0}')" = '[]' ] || { echo 'Apply mode unavailable: existing tc classes require manual review.' >&2; return 1; }
 [ "$(printf '%s' "$apply_filters" | awk '{gsub(/[[:space:]]/,"");printf "%s",$0}')" = '[]' ] || { echo 'Apply mode unavailable: existing tc filters require manual review.' >&2; return 1; }
 apply_qdisc=$(/usr/sbin/tc -j qdisc show dev "$apply_interface") || { echo 'Apply mode unavailable: qdisc inspection failed.' >&2; return 1; }
 apply_qdisc_compact=$(printf '%s' "$apply_qdisc" | awk '{gsub(/[[:space:]]/,"");printf "%s",$0}')
 case "$apply_qdisc_compact" in
  *'"parent":'*) echo 'Apply mode unavailable: child qdiscs require manual review.' >&2; return 1;;
 esac
 case "$apply_qdisc_compact" in
  *'"kind":"noqueue"'*'"handle":"0:"'*'"root":true'*) apply_replace_qdisc=false; apply_qdisc_fingerprint=;;
  *'"kind":"fq"'*'"handle":"0:"'*'"root":true'*) apply_replace_qdisc=true; apply_qdisc_fingerprint=$(printf '%s\n' "$apply_qdisc" | sha256sum | awk '{print $1}');;
  *) echo 'Apply mode unavailable: only an empty noqueue or kernel fq/0: root qdisc can be adopted.' >&2; return 1;;
 esac
 apply_nft=$(/usr/sbin/nft list ruleset) || { echo 'Apply mode unavailable: nftables inspection failed.' >&2; return 1; }
 case "$apply_nft" in *flowtable*|*'flow offload'*|*'meta mark'*|*'ct mark'*) echo 'Apply mode unavailable: flowtable/offload or packet-mark policy conflicts with RelaDock.' >&2; return 1;; esac
 apply_rules=$(/usr/sbin/ip -4 rule show) || { echo 'Apply mode unavailable: policy-rule inspection failed.' >&2; return 1; }
 case "$apply_rules" in *fwmark*) echo 'Apply mode unavailable: an existing fwmark policy requires manual review.' >&2; return 1;; esac
 apply_old_ip_forward=$(cat /proc/sys/net/ipv4/ip_forward) || { echo 'Apply mode unavailable: IPv4 forwarding state is unavailable.' >&2; return 1; }
 apply_features=$(/usr/sbin/ethtool -k "$apply_interface") || { echo 'Apply mode unavailable: offload inspection failed.' >&2; return 1; }
 apply_old_gro=$(printf '%s\n' "$apply_features" | awk -F': ' '$1=="generic-receive-offload" {split($2,a," ");print a[1]}')
 apply_old_gso=$(printf '%s\n' "$apply_features" | awk -F': ' '$1=="generic-segmentation-offload" {split($2,a," ");print a[1]}')
 apply_old_tso=$(printf '%s\n' "$apply_features" | awk -F': ' '$1=="tcp-segmentation-offload" {split($2,a," ");print a[1]}')
 apply_old_lro=$(printf '%s\n' "$apply_features" | awk -F': ' '$1=="large-receive-offload" {split($2,a," ");print a[1]}')
 for apply_feature in "$apply_old_gro" "$apply_old_gso" "$apply_old_tso" "$apply_old_lro"; do case "$apply_feature" in on|off) :;; *) echo 'Apply mode unavailable: incomplete offload inspection.' >&2; return 1;; esac; done
 printf '%s' 'Link shaping ceiling in Mbps [1000]: ' >&3
 IFS= read -r apply_link_mbps <&3 || apply_link_mbps=
 [ -n "$apply_link_mbps" ] || apply_link_mbps=1000
 case "$apply_link_mbps" in ''|*[!0-9]*) echo 'Apply mode unavailable: link ceiling must be an integer.' >&2; return 1;; esac
 [ "$apply_link_mbps" -ge 1 ] && [ "$apply_link_mbps" -le 100000 ] || { echo 'Apply mode unavailable: link ceiling must be between 1 and 100000 Mbps.' >&2; return 1; }
 printf '%s\n' '' 'Reviewed apply proposal:' "  node: $apply_node" "  interface: $apply_interface (ifindex $apply_ifindex)" "  local SNAT/listen address: $apply_snat" "  namespace: $apply_namespace" "  replace empty default qdisc: $apply_replace_qdisc" '  packet mark namespace: 82' "  shaping ceiling: $apply_link_mbps Mbps (not guaranteed bandwidth)" >&3
 printf '%s' 'Type ENABLE to confirm: ' >&3
 IFS= read -r apply_confirmation <&3 || apply_confirmation=
 [ "$apply_confirmation" = ENABLE ] || { echo 'Apply mode was not confirmed; validation-only remains active.'; return 1; }

 install -o root -g root -m 0600 /etc/reladock/agent.env /etc/reladock/agent.env.pre-apply || { echo 'Cannot back up agent.env; validation-only remains active.' >&2; return 1; }
 apply_preparation_active=1
 awk '!/^RELADOCK_(STATE_DIR|SNAT|SHAPING_INTERFACE|WAL_DIR|SOURCE_FLOW_WAL_DIR|NFT_BINARY|TC_BINARY|IP_BINARY|ETHTOOL_BINARY|SHAPING_LEASE)=/' /etc/reladock/agent.env > "$temp/agent.env.apply"
 printf '%s\n' 'RELADOCK_STATE_DIR=/var/lib/reladock-agent' "RELADOCK_SNAT=$apply_snat" "RELADOCK_SHAPING_INTERFACE=$apply_interface" 'RELADOCK_WAL_DIR=/var/lib/reladock-agent/wal' 'RELADOCK_SOURCE_FLOW_WAL_DIR=/var/lib/reladock-agent/source-flow-wal' 'RELADOCK_NFT_BINARY=/usr/sbin/nft' 'RELADOCK_TC_BINARY=/usr/sbin/tc' 'RELADOCK_IP_BINARY=/usr/sbin/ip' 'RELADOCK_ETHTOOL_BINARY=/usr/sbin/ethtool' 'RELADOCK_SHAPING_LEASE=/etc/reladock/shaping.json' >> "$temp/agent.env.apply"
 install -o root -g root -m 0600 "$temp/agent.env.apply" /etc/reladock/agent.env || { if rollback_apply_preparation; then apply_preparation_active=0; echo 'Cannot install completed agent.env; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Environment installation and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi; }
 printf '{"Device":"%s","IfIndex":%s,"NamespaceID":"%s","OwnerID":"%s","Dedicated":true,"ReplaceDefaultQdisc":%s,"DefaultQdiscFingerprint":"%s","MarkNamespace":82,"LinkMbps":%s}\n' "$apply_interface" "$apply_ifindex" "$apply_namespace" "$apply_node" "$apply_replace_qdisc" "$apply_qdisc_fingerprint" "$apply_link_mbps" > "$temp/shaping.json"
 install -o reladock -g reladock -m 0600 "$temp/shaping.json" /etc/reladock/shaping.json || { if rollback_apply_preparation; then apply_preparation_active=0; echo 'Cannot install the shaping lease; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Shaping lease installation and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi; }
 install -o root -g root -m 0600 /dev/null /etc/reladock/network-managed || { if rollback_apply_preparation; then apply_preparation_active=0; echo 'Cannot record network authorization; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Network authorization and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi; }
 if ! systemctl enable --now reladock-agent-network.service || ! systemctl is-active --quiet reladock-agent-network.service; then
  if rollback_apply_preparation; then apply_preparation_active=0; echo 'Network prerequisite preparation failed; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Network prerequisite preparation and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi
 fi
 if ! /opt/reladock/reladock-agent preflight-apply --node "$apply_node" --state-dir /var/lib/reladock-agent --snat "$apply_snat" --shaping-interface "$apply_interface" --metering-wal-dir /var/lib/reladock-agent/wal --source-flow-wal-dir /var/lib/reladock-agent/source-flow-wal --nft-binary /usr/sbin/nft --tc-binary /usr/sbin/tc --ip-binary /usr/sbin/ip --ethtool-binary /usr/sbin/ethtool --shaping-lease /etc/reladock/shaping.json; then
  if rollback_apply_preparation; then apply_preparation_active=0; echo 'Apply preflight failed; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Apply preflight and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi
 fi
 if ! systemctl disable --now reladock-agent.service; then
  if rollback_apply_preparation; then apply_preparation_active=0; echo 'Could not stop validation mode; validation-only was restored.' >&2; return 1; else apply_preparation_active=0; apply_preserve_temp=1; echo "Validation stop and rollback need operator attention; preserve /etc/reladock/agent.env.pre-apply and $temp." >&2; return 3; fi
 fi
 # Starting apply is the no-automatic-rollback boundary: it may claim and
 # modify a configuration immediately, so every private artifact is retained.
 apply_preparation_active=0
 if ! systemctl enable --now reladock-agent-apply.service || ! systemctl is-active --quiet reladock-agent-apply.service; then
  echo 'Apply Agent failed after the application boundary. Validation remains disabled; preserve PKI, journal, state and WAL, then use the documented recovery procedure.' >&2
  return 2
 fi
 rm -f /etc/reladock/agent.env.pre-apply
 echo 'Agent started in explicit apply mode. Wait for an applied ACK; service activity is not end-to-end forwarding health.'
 return 0
}

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
install -o root -g root -m 0644 "$temp/release/reladock-agent-network.service" /etc/systemd/system/reladock-agent-network.service
install -o root -g root -m 0644 "$temp/release/reladock-updater.service" /etc/systemd/system/reladock-updater.service
install -o root -g root -m 0644 "$temp/release/reladock-updater.timer" /etc/systemd/system/reladock-updater.timer
systemctl daemon-reload
systemctl enable --now reladock-agent
systemctl is-active --quiet reladock-agent || fail 'Agent failed to start. Enrollment retained; inspect journalctl -u reladock-agent'
systemctl enable --now reladock-updater.timer
systemctl is-active --quiet reladock-updater.timer || fail 'Updater timer failed to start. Agent remains installed; inspect journalctl -u reladock-updater.timer'
if (exec 3<>/dev/tty) 2>/dev/null; then
 exec 3<>/dev/tty
 printf '%s\n' '' 'Fresh Agent enrollment completed.' '1) Enable forwarding (apply mode)' '2) Keep validation-only (default)' >&3
 printf '%s' 'Select [1/2]: ' >&3
 IFS= read -r bootstrap_fresh_selection <&3 || bootstrap_fresh_selection=
 if [ "$bootstrap_fresh_selection" = 1 ]; then
  apply_result=0
  prepare_fresh_apply || apply_result=$?
  exec 3>&-
  if [ "$apply_result" -eq 2 ] || [ "$apply_result" -eq 3 ]; then exit 1; fi
  [ "$apply_result" -eq 0 ] && exit 0
 else
  exec 3>&-
 fi
fi
echo 'Agent started in validation-only mode. Check contact and configuration results in the console. Forwarding is NOT enabled.'
