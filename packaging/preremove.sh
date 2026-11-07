#!/bin/sh
# Pre-remove of the deb and rpm packages: stops and disables the service when the
# package is removed, not when it is upgraded (dpkg passes "remove" or
# "deconfigure" and "upgrade" on an upgrade; rpm passes 0 on erase and 1 on an
# upgrade). Data and the service user are never deleted.
set -eu

case "${1:-}" in
  remove | deconfigure | 0) ;;
  *) exit 0 ;;
esac

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  systemctl stop mongorescue.service || true
  systemctl disable mongorescue.service >/dev/null 2>&1 || true
fi
exit 0
