# clowder

A small cluster orchestrator for Docker Compose stacks on hosts with about 4 GB of RAM:
replicated volumes (DRBD 9), the real client IP behind a Floating IP, priorities, fencing
through the hoster's API. First hoster: Hetzner Cloud.

Status: phase 0. The repository holds only the spike in [`spike/`](spike/README.md),
throw-away scripts that check the risky paths before the code is written.
