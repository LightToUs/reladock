#!/usr/bin/env bash
set -euo pipefail

backup_dir=/root/reladock-backups/2026-11-01-public-site-v1/letsencrypt-renewal
webroot=/var/www/letsencrypt
hook_source=/root/reload-nginx-after-renewal.sh
hook_target=/etc/letsencrypt/renewal-hooks/deploy/reload-nginx

install -d -m 0700 "$backup_dir"
install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy

configure_renewal() {
  renewal_file=$1
  shift

  test -f "$renewal_file"
  backup_file="$backup_dir/$(basename "$renewal_file")"
  if [ ! -e "$backup_file" ]; then
    cp -a "$renewal_file" "$backup_file"
  fi

  sed -i 's/^authenticator = standalone$/authenticator = webroot/' "$renewal_file"
  if ! grep -q '^webroot_path = ' "$renewal_file"; then
    sed -i "/^authenticator = webroot$/a webroot_path = $webroot" "$renewal_file"
  fi

  if ! grep -q '^\[\[webroot_map\]\]$' "$renewal_file"; then
    {
      printf '\n[[webroot_map]]\n'
      for domain_name in "$@"; do
        printf '%s = %s\n' "$domain_name" "$webroot"
      done
    } >> "$renewal_file"
  fi
}

configure_renewal /etc/letsencrypt/renewal/reladock.com.conf \
  reladock.com www.reladock.com
configure_renewal /etc/letsencrypt/renewal/console.reladock.com.conf \
  console.reladock.com

install -m 0755 "$hook_source" "$hook_target"

grep -q '^authenticator = webroot$' /etc/letsencrypt/renewal/reladock.com.conf
grep -q '^authenticator = webroot$' /etc/letsencrypt/renewal/console.reladock.com.conf
test -x "$hook_target"
