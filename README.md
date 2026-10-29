# clowder

A small cluster orchestrator for Docker Compose stacks on hosts with about 4 GB of RAM:
replicated volumes (DRBD 9), the real client IP behind a Floating IP, priorities, fencing
through the hoster's API. First hoster: Hetzner Cloud.

Status: under construction (phase 1a, see the roadmap in [DEVELOPING.md](DEVELOPING.md)).
What runs: on a host `clowderd check` and `clowderd init`, which starts the agent (with the
containers' isolation and DNS resolver); from the CLI `clowder cluster
bootstrap|apply|diff|status|schema`, `clowder host ...` and `clowder stack
create|apply|delete|ps|logs`, which runs Compose stacks with their volumes and secrets on
one host.

A stack's passwords live in a SOPS file next to its `clowder.yaml`; the CLI decrypts it
with your age key, and the cluster keeps the values sealed. Who can read them: whoever can
decrypt that file, every host of the cluster, and, until clowder has permissions, every user
through a stack that `cluster.yaml` allows privileges (`stacks.privileged`).

| Binary | For | Platforms |
|---|---|---|
| `clowder` | the CLI | Linux, macOS, Windows |
| `clowderd` | the agent on each host | Debian 13 (amd64) |

Building and the layout of the code: [DEVELOPING.md](DEVELOPING.md).
