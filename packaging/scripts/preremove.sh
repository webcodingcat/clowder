#!/bin/sh
# Stop the agent before its binary goes away. Containers keep running; a
# reinstall or `clowderd join` brings the agent back.
set -e
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
  systemctl stop clowderd.service || true
fi
