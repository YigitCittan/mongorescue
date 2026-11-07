#!/bin/sh
# Post-install of the deb and rpm packages: creates the mongorescue service user
# and its state directory. Idempotent: safe on install, upgrade and reinstall.
# Works for dpkg (configure) and rpm; it never touches existing data.
set -eu

user=mongorescue
home=/var/lib/mongorescue

if ! getent group "$user" >/dev/null 2>&1; then
  groupadd --system "$user"
fi
if ! getent passwd "$user" >/dev/null 2>&1; then
  nologin=/usr/sbin/nologin
  [ -x "$nologin" ] || nologin=/sbin/nologin
  useradd --system --gid "$user" --home-dir "$home" --no-create-home \
    --shell "$nologin" --comment "MongoRescue service" "$user"
fi

# The unit also creates it (StateDirectory=), so that offline commands such as
# "sudo -u mongorescue mongorescue key status" work before the first start.
mkdir -p "$home"
chown "$user:$user" "$home"
chmod 0750 "$home"

# Only where systemd runs (not in a container build or chroot).
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
  # An upgrade restarts a running service to load the new binary; a fresh
  # install does not start anything: the dashboard is opened deliberately.
  if systemctl is-active --quiet mongorescue.service; then
    systemctl restart mongorescue.service || true
  else
    echo "MongoRescue is installed. Start it with: systemctl enable --now mongorescue"
    echo "Bootstrap options: /etc/default/mongorescue. Setup code: journalctl -u mongorescue"
  fi
fi
exit 0
