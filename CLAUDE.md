# clowder

Cluster orchestrator for Docker Compose stacks (Go, two binaries: `clowder` and `clowderd`).
Everything in this repo is English: code, comments, docs, CLI output, commit messages.

Current state: phase 0. Only `spike/` exists: Bash scripts that try the risky paths
(WireGuard per peer, DNAT without SNAT, DRBD 9 quorum and fencing, boot behaviour) on three
VMs of HLab, a local Hetzner Cloud lab. `spike/README.md` says how to run them. The spike is
throw-away code and is removed at the end of phase 0; the repository layout, `DEVELOPING.md`
and `STYLE.md` come with phase 1a.

Rules that already apply:

- Bash follows the Google Shell Style Guide (`shfmt -i 2 -ci -bn`, `shellcheck`).
- OpenTofu files follow `tofu fmt`.
- Commits: imperative subject (~50 characters), blank line, why.
