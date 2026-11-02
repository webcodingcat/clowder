#!/bin/sh
# Reload systemd so it knows the units. The agent is started by
# `clowderd init` or `clowderd join`, not by the package. An upgrade
# (configure with the old version in $2) restarts a running agent, so the new
# binary runs; KillMode=process leaves its helpers and the containers alone.
set -e
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
  if [ "$1" = "configure" ] && [ -n "$2" ]; then
    systemctl try-restart clowderd.service
  fi
fi
