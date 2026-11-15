#!/usr/bin/env bash
set -euo pipefail

backup_dir=/root/reladock-backups/2026-11-01-public-site-v1
control_env=/etc/reladock/control.env

rollback() {
  echo 'cutover failed; restoring direct console listener' >&2
  systemctl stop nginx || true
  cp -a "$backup_dir/control.env" "$control_env"
  systemctl restart reladock
}

on_exit() {
  exit_code=$?
  if [ "$exit_code" -ne 0 ]; then
    rollback
  fi
  exit "$exit_code"
}

trap on_exit EXIT

test -r "$backup_dir/control.env"
grep -qx 'LISTEN_ADDRESS=0.0.0.0:443' "$control_env"
nginx -t

sed -i 's/^LISTEN_ADDRESS=0\.0\.0\.0:443$/LISTEN_ADDRESS=127.0.0.1:8443/' "$control_env"
systemctl restart reladock

for attempt in 1 2 3 4 5; do
  if curl --silent --show-error --fail \
    --connect-to console.reladock.com:443:127.0.0.1:8443 \
    https://console.reladock.com/ -o /dev/null; then
    break
  fi
  if [ "$attempt" -eq 5 ]; then
    echo 'loopback console did not become ready' >&2
    exit 1
  fi
  sleep 1
done

systemctl start nginx
systemctl is-active --quiet nginx

curl --silent --show-error --fail \
  --resolve reladock.com:443:127.0.0.1 \
  https://reladock.com/ -o /dev/null
curl --silent --show-error --fail \
  --resolve console.reladock.com:443:127.0.0.1 \
  https://console.reladock.com/ -o /dev/null

www_code=$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  --resolve www.reladock.com:443:127.0.0.1 \
  https://www.reladock.com/)
test "$www_code" = '308'

test "$(sed -n 's/^AGENT_LISTEN_ADDRESS=//p' "$control_env")" = '0.0.0.0:9443'
ss -lnt | grep -q '127.0.0.1:8443'
ss -lnt | grep -q ':9443'
ss -lnt | grep -q ':443'

trap - EXIT
echo 'cutover complete'
