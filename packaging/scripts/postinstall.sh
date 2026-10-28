#!/bin/sh
# Reload systemd so it knows the units. The agent is started by
# `clowderd init` or `clowderd join`, not by the package.
set -e
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
fi
