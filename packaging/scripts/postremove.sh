#!/bin/sh
# Make systemd forget the removed units.
set -e
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
fi
