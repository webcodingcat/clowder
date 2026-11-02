#!/bin/sh
# Reload systemd so it knows the units. The agent is started by
# `clowderd init` or `clowderd join`, not by the package. An upgrade
# (configure with the old version in $2) restarts a running agent, so the new
# binary runs; KillMode=process leaves its helpers and the containers alone.
# It also starts an agent that failed, whatever the reason: one that stopped
# at a Raft entry it could not read stays down until the new version reads
# it. A stopped agent stays stopped.
set -e
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
  if [ "$1" = "configure" ] && [ -n "$2" ]; then
    if systemctl is-failed --quiet clowderd.service; then
      systemctl restart clowderd.service
    else
      systemctl try-restart clowderd.service
    fi
  fi
fi
