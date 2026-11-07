#!/bin/sh
# Post-remove of the deb and rpm packages: reloads systemd after the unit file is
# gone. The data directory /var/lib/mongorescue, the service user and
# /etc/default/mongorescue (dpkg keeps conffiles until purge) stay on purpose:
# they hold the secret key and the metadata of the backups.
set -eu

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
fi
exit 0
