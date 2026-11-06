# Operating a cluster

How to run a clowder cluster after [Getting started](../README.md#getting-started-on-hetzner-cloud):
credentials, the sealing key, hosts, failures, updates, and the limits you should know.
[resources.md](resources.md) describes the files you apply.

## Credentials

A cluster has no users. Whoever holds a context of the CLI's credentials controls the
cluster, as with kubeadm's `admin.conf`.

- **Get credentials** from root on a control-plane host:
  `ssh root@<host> clowderd credentials | clowder config import -`. Check the host's SSH
  key on first contact, because the credentials include the CA that the CLI trusts.
  `--name <name>` names the certificate in the audit (default `admin`); the name grants
  nothing.
- **Contexts.** `clowder config import` adds one context to `~/.clowder/config` (or
  `--config`, `$CLOWDER_CONFIG`), named after the cluster or `--as <name>`.
  `clowder config get-contexts`, `use-context` and `delete-context` manage them, and
  `--context` picks one for a single command.
- **Renew.** A CLI certificate lasts a year, and every command warns before it ends. Run
  `clowderd credentials` again and import the result with `--replace`.
- **Lock out old certificates.** `clowderd credentials --revoke-older` locks out every CLI
  certificate issued before the new one, for example after a laptop was lost. It needs a
  leader.
- **Endpoints.** The CLI reaches the cluster only through the `endpoints` of its context,
  which `clowderd credentials` fills with the control-plane hosts' public addresses at
  that time. After a control-plane host joins or goes, fetch new credentials or edit the
  endpoints (`clowder host list` shows the addresses). `--endpoint` sets your own, such as
  a load balancer in front of the hosts.

## The sealing key

Every SealedSecret is sealed for the cluster's sealing key.

- **Export it right after setup** and keep it offline:
  `ssh root@<host> clowderd sealing-key export > sealing-key`. Whoever holds it can read
  every SealedSecret in your files.
- `clowderd sealing-key export --public` prints the public half. `clowder secret seal
  --cert <file>` seals with it, without a cluster.
- **If the cluster is lost,** the sealing key still opens the values in your files:
  `clowder secret decrypt --identity sealing-key -f <file>` prints them, and
  `clowder secret rekey -f <dir> --identity sealing-key` seals them again for the cluster
  of the current context (or for `--cert`).

## Hosts and roles

A host has roles, as a Kubernetes node does:

| Role | Does |
|---|---|
| `control-plane` | votes in Raft, leads, serves the CLI and the join, holds the cluster's keys |
| `worker` | runs stacks and holds volume copies with a disk |
| `ingress` | may take the Floating IP |

- **Join.** `clowder host join-token` makes a token that opens once, for an hour (`--ttl`
  up to a day). It allows `worker,ingress` unless `--roles` says more, so a token for a
  control-plane host needs `--roles control-plane,worker,ingress`. The roles are fixed
  once the host has joined. Hosts join only once the ClusterConfiguration is applied.
- **Votes.** 1, 3 or 5 control-plane hosts vote. The second control-plane host waits for
  its vote until a third joins, because two voters would stop when either fails.
- **Drain** (`clowder host drain <host>`) moves a host's stacks off it for maintenance and
  keeps new ones off; its volume copies stay. `host undrain` takes it back.
- **Remove** (`clowder host remove <host>`) asks first. A host that still has stacks or
  copies drains first and is removed once it is empty. Once three control-plane hosts were
  in service, clowder never goes below three: a replacement joins first. A removed host's
  certificate is revoked; wipe its disk ([Known limits](#known-limits)).
- **Give each cluster a Hetzner project of its own.** The Hetzner token can power off
  every server of its project.

## When a host fails

- **Failover.** When the voters stop hearing a host with stacks for 45 s, the leader
  powers its server off through Hetzner's API and starts its stacks on a host with a
  synced copy of their data. A stack with a volume of `replicas: 1` waits until the host
  is back; a volume of `replicas: 0` starts anew, empty.
- **The server cannot be powered off.** The alert `fencing-blocked` says so. Once you know
  the server is off, `clowder host fence-confirm <host>` lets its stacks start elsewhere.
- **A host that keeps failing** is fenced and not powered on again after a few fences in a
  short time (alert `host-disabled`). `clowder host enable <host>` powers it on again.
- **Split brain.** After the alert `split-brain`, choose the copy that keeps its data:
  `clowder volume resolve-splitbrain <namespace>/<volume> --keep <host>`. The other copies
  take it over.
- **The majority of the control-plane hosts is gone for good** (two of three).
  The cluster decides nothing. Run `clowderd recover --power-off` as root on the
  control-plane host that is left. It refuses while another host still knows a leader. It
  labels and powers off the servers of the other hosts, asks for this host's name to
  confirm, and goes on with this host alone. The alert `no-ha` stays until three
  control-plane hosts are in service again. Set the retired servers up anew or delete
  them.

The alerts and when they are raised are listed in
[loops.md](developing/loops.md#alerts-internalnotify-controllerwantedgo-controlleralertsgo).
`clowder alert test` sends a test alert to every channel.

## Updating a cluster

Before `v1.0.0`, the CLI and the agents work together only within one minor version.

- **A patch release** (`0.2.0` to `0.2.3`) goes host by host: the followers first, one
  after another, and the leader last (`clowder cluster status` names it). Move on to the
  next host only once the updated one is back; `clowder host list` shows its version.
- **A new minor version:**
  1. Stop the agent on every host (`systemctl stop clowderd`; the containers keep
     running).
  2. Install the new package on every host. An upgrade does not start a stopped agent.
  3. Start the agents again and update the CLI.
- A release before `v1.0.0` may change the state or a format without a way to upgrade a
  running cluster. Such a release needs the cluster set up anew, and it refuses
  credentials of an earlier build.

The order keeps the cluster working. An agent that meets a log entry it cannot read stops
instead of skipping it, but this does not make mixing minor versions safe.

## Known limits

**Keys and access**

- Every host holds the data key, and with it the CA's key, the sealing key and so every
  secret. Root on any host, a worker too, can issue itself any certificate.
- A removed host keeps these keys. A host that kept its disk can still issue certificates
  that the revocations do not name. Wipe a removed host's disk. A new sealing key would
  need every file sealed anew with `secret rekey`; there is no key rotation yet.
- The CLI checks an agent's certificate against the CA, not against the revocations. So a
  removed host could answer the CLI as the cluster, with a sealing key of its own for
  `secret seal`, or take the values of `create secret` and of a `Secret` in clear. Remove
  a host's address from your contexts' endpoints when the host goes.
- Hosts without the control plane follow the Raft log, so they see every spec.
- Whoever holds the CLI's credentials can check a guessed value against a secret's
  digest. They can read the value through a privileged stack anyway.

**Hosts**

- A host's roles are fixed once it has joined. To change them, remove the host and let it
  join anew.
- With one control-plane host, a worker that restarts while that host is down starts none
  of its stacks; it waits for the leader. The stacks that run keep running.
- A drained host that fails comes back active, not drained.
- While the leader tries to power off a failed host (up to a few minutes when Hetzner's
  API does not answer), it notices no other failure. A second host that fails then is
  fenced late.
- A disabled voter keeps its vote until it is removed.
- If no replacement can join, a cluster that had three control-plane hosts stays at two.
  `clowderd recover --power-off` is the way out only when two are gone.
- A retired host whose agent starts while Hetzner's API does not answer starts as before,
  so that an outage at Hetzner does not keep hosts down. Set retired servers up anew or
  delete them.
- `clowderd recover` knows only the hosts that its own log knows. Power off by hand a host
  that the others added after this one fell behind.
- `examples/hetzner` ignores later changes to a server's user data, so a storage class
  added later gets no thin pool on an existing host.
- Images are never pruned, and every host pulls the images of the stacks it may take
  over, so the root file system fills up over time (alert `root-use`).

**Volumes**

- There are no backups: a volume's data is only on its hosts' disks.
- `replicas` cannot change after a volume is created.
- After a recover, a volume without a synced copy on the recovering host waits for a
  restore. Deleting it and applying it anew gives an empty volume. An unsynced copy on the
  recovering host stays as it is, for you to handle.
- With two workers and no third host, a volume of replicas 1 that moves has two copies
  without a tiebreaker for the length of the move. If either host fails then, the volume's
  writes stop until the host is back.
- A host that comes back after a fence resyncs all its copies at once, each at the resync
  rate.
- A failed host that is not fenced keeps its copies counted, and no copy is made anew
  elsewhere until you run `host fence-confirm` (alert `fencing-blocked`). This happens when
  the host needs no fence, no ClusterConfiguration is applied yet, the server is not
  confirmed, or Hetzner's API is down for good.
- `volume resolve-splitbrain`, and `clowderd recover` on a host that rebooted after the
  others went, never ran on real DRBD, only in unit tests.

**The CLI and the network**

- The CLI learns no hosts from the cluster: keep the endpoints current
  ([Credentials](#credentials)).
- `clowder get` right after `clowder apply` may show, for a moment, the state of a host
  that has not applied the apply yet.
- If a stack with a published port moved while the agent of the ingress host was down, the
  port keeps its open connections to the old host until they end.
- The question before a new Hetzner token checks at most 50 servers. In a larger project,
  the answer says it could not check.
- A Prometheus in a stack or outside the cluster cannot reach a host's `/metrics`; only
  the hosts reach it.
