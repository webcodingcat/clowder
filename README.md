# clowder

A small cluster orchestrator for Docker Compose stacks on hosts with about 4 GB of RAM:
replicated volumes (DRBD 9), the real client IP behind a Floating IP, priorities, fencing
through the hoster's API. First hoster: Hetzner Cloud.

Status: under construction (phase 1a, see the roadmap in [DEVELOPING.md](DEVELOPING.md)).
What runs: `clowderd check` on a host. The CLI has `cluster status` and `host ...`, but
there is no agent to answer them yet (`clowderd run` comes with `clowderd init`).

| Binary | For | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

Building and the layout of the code: [DEVELOPING.md](DEVELOPING.md).
