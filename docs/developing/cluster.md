# Certificates, Raft and Recover

Part of [DEVELOPING.md](../../DEVELOPING.md): how hosts trust each other, how the state is
replicated, and how a cluster that lost its majority goes on. The state itself is in
[state.md](state.md), the API in [api.md](api.md).

## Certificates (`internal/pki`)

- **Who a certificate names.** The cluster CA issues host certificates (server and
  client, valid for the host's addresses) and CLI certificates (client). The role is the
  certificate's organizational unit, and the name is its common name: `host-<id>`, or the
  CLI's name (`clowderd credentials --name`, or `DefaultCLIName` without one, as kubeadm's
  `admin.conf` is `kubernetes-admin`). The name must pass `names.Valid`. A CLI name is for
  the audit and grants nothing.
- **A host's roles are in its certificate,** as its organizations (`Identity.HostRoles`),
  so another host can tell them without the state. Kubernetes keeps node roles in labels;
  clowder puts them in the certificate so that a host's calls can be checked from the
  certificate alone.
- **Keys** are Ed25519, and the CA signs public keys. A host's key never leaves the host.
  `clowderd credentials` creates the CLI's key on a host and hands it over in a CLI config,
  as kubeadm does with `admin.conf`.
- **A host certificate ends by revocation, not by expiry,** because hosts may stay off for
  months (`leafLifetime`). `Verify` takes the revoked serials from the state, which keeps
  each one until a while after it expired.
- **A CLI certificate** lasts `cliLifetime` (a year, like kubeadm's client certificates),
  or ends sooner through the CLI cutoff: the API compares its start (`Identity.NotBefore`)
  with `State.CLICutoff`. Nothing renews it. Every CLI command warns from `renewWarning`
  before its end (`cli/connect.go`), and the user fetches a new one from a host.
- **`TrustProof`** (HMAC-SHA256 over the CA certificate) is how a joining host trusts the
  CA on first contact. Its key is the join token's hash (`jointoken.ProofKey`). The CLI
  needs no proof, because its first credentials come over SSH with the CA included.
- **Three TLS configs** check hosts by the chain and the role, never by the name:

  | Config | Used by |
  |---|---|
  | `ClientTLSToHosts` | an agent that calls a host of any role, as the leader calls a worker |
  | `ClientTLSToControlPlane` | the CLI, a joining host and clowderd's own commands, so that a worker, even a removed one, cannot answer as the cluster |
  | `ServerTLSForControlPlane` | Raft, and the API of a host without the control plane |

  `tls_test.go` tests them against every peer they must refuse: none, the CLI, a host of
  another CA, a stranger showing a host's certificate behind its own, a revoked, an expired
  and a not yet valid host, and a worker where only the control plane may answer or call.
  It runs the handshakes on 127.0.0.1.

## Raft (`internal/raftnode`) and the data key (`internal/datakey`)

- **The member.** `raftnode` runs hashicorp/raft, with the log and stable store in BoltDB,
  a snapshot every `snapshotThreshold` entries, and the state of `internal/state` as the
  replicated machine. Raft's own log lines go through `log/slog`.
- **mTLS on the node address.** Both sides present a host certificate of the cluster CA
  that is not revoked.
  - Every host's listener takes only control-plane hosts (`pki.ServerTLSForControlPlane`).
    Only the leader and the candidates send, and they are voters. A worker that got in
    could send entries of its own past every check of the API.
  - The dial takes any host (`pki.ClientTLSToHosts`), because the leader also sends to the
    hosts that follow the log without a vote.
  - CLI certificates, workers and other clusters' hosts fail the handshake
    (`TestOnlyControlPlaneHostsOfTheClusterGetIn`).
  - Removing a host revokes its certificate. The API checks the revocations with every
    request, so an open keep-alive connection gains nothing. Raft checks them at the
    handshake, and a removed host also leaves Raft's configuration, so the leader no longer
    sends to it.

  Kubernetes has no such listener on its workers. clowder's hosts follow the log
  themselves instead of watching an API server, which keeps a small cluster to one process
  per host.
- **Leading starts with a barrier.** `followLead` sets `IsLeader` only once Raft's barrier
  has applied every entry from before the lead. A new leader may not yet have applied what
  its predecessor committed, and its loops and `runOnLeader` would then decide on an old
  state. `VerifyLeader` asks a majority whether this member still leads. The leader asks
  before it powers a server off or moves the Floating IP, because a leader cut off from
  the others believes that it leads until its lease runs out.
- **Catching up.** `CatchUp` waits for a leader (and, on the leader, for the barrier). A
  follower then asks for the leader's commit index (`api.Server.CatchUpWithLeader`,
  `/v1/internal/commit-index`, answered after `VerifyLeader`) and waits until its state
  holds that index (`WaitCommitted`). Raft's own entries never reach the state, so it waits
  for Raft to have applied the index, and then for the last command up to it.
  - The agent starts its stacks and volumes only after that
    ([The agent](host.md#the-agent-internalagent)), because a host that was away must not
    start a stack that moved in the meantime.
  - Until then, the API holds users' calls for at most `waitTimeout`
    (`ServerConfig.CaughtUp`), because a state that has not caught up may still be empty
    and answer that the cluster is not bootstrapped. The hosts' own routes do not wait,
    because the catch-up, the beats and the joins use them.
- **Using the state.** `Propose` commits a command through the leader and returns its
  index, or the state's refusal. `Read` gives the current state under a lock; no caller may
  keep it, because `Apply` changes it in place. `WaitIndex` lets a read follow a write made
  through another member. `Changes` serves the watch from the member's ring of changes
  ([The watch](api.md#the-watch)). `SharedCopy` is the clone that the loops read
  ([Copies of the state](state.md#copies-of-the-state)).
- **Members.** `Bootstrap` makes the first host a cluster of its own; on a member that has
  a log it does nothing. The leader changes the members one at a time (`members.go`:
  `AddNonvoter`, `AddVoter`, `DemoteVoter`, `RemoveMember`, `TransferLeadership`), from the
  members loop (`controller/members.go`). Each pass makes one change, in this order
  (`nextMemberStep`):
  1. A leader that is being removed hands over its lead. The API does that before it
     commits the removal (`handOverLeadOf`), because the removal revokes the leader's
     certificate. If the state then refuses the removal, the lead has moved for nothing,
     which does no harm. The loop is the fallback for a removal that the API did not see
     coming: the others accept no new connection from a revoked leader, but Raft's open
     connections stay, and so would its lead.
  2. Every host that is not removed is a member; a joining host joins as a non-voter. A
     joining host, or a host without the control plane, that votes loses its vote. This
     happens to a host that took the ID of a removed host before that host left Raft: it
     finds the old host's vote.
  3. A joining host whose agent reported (`report-host`) becomes `active`.
  4. Of the control-plane hosts in service or expected back after a failure (`votes`:
     active, draining, failed, fencing, fenced, recovering, disabled), 1, 3 or 5 vote
     (`voterCount`): first those that already vote, then those with the lowest IDs.
     - The second control-plane host waits for its vote until a third comes; if it is a
       worker, it runs stacks meanwhile.
     - Only a host in service gets a new vote. The other hosts follow the log as
       non-voters, so their loops read the state as a voter's do.
     - A failed voter keeps its vote, because taking it would take a live host's vote
       instead: with three hosts, the cluster would stop at the next failure. The leader
       keeps its own vote.
  5. A removed host leaves Raft, after step 4 has given its vote to another host.
  6. `set-host-voter` mirrors each host's vote in the state, which shows it.

  Raft changes its members one by one, so the third host's join passes through a few
  seconds with two voters. During those seconds, the loss of either one stops the cluster
  until it is back. The leader handles one join at a time (`joiner.mu`), so two hosts
  never get the same ID.
- **The data key** is an age identity in a file that only root can read, on control-plane
  hosts only. `Encrypt` and `Decrypt` turn secrets into `state.Encrypted` and back. A
  joining control-plane host receives the key with its join answer; any other host gets
  none and asks the leader for what it may read
  ([api.md](api.md#keys-only-on-the-control-plane)).
- **For recover.** `ReadState` reads the state from the log on disk without Raft (the last
  snapshot, then the commands after it). `Recover` makes the member its cluster's only
  voter (`raft.RecoverCluster`). Both are for `clowderd recover`, while the agent is
  stopped.
- **Tests** run real members on 127.0.0.1: a restart from disk, three hosts over mTLS,
  strangers refused, and a member that recovers alone.

## Recover (`clowderd recover`, `agent/recover.go`)

A cluster that has lost the majority of its voters for good decides nothing, but the
control-plane host that is left still has current copies of its volumes. `clowderd
recover --power-off` lets it go on alone, and only on a human's word.

The danger is the hosts that are gone. If they were switched on again, two of them would
still have the old Raft configuration's majority, and their copies would have DRBD's
quorum. They would elect a leader and fence the host that recovered. So `Recover` takes
them out at Hetzner, in this order. Each step is safe to run again after a failure.

1. It stops `clowderd.service` and reads the state from the log on disk
   (`raftnode.ReadState`). The hosts to retire are all hosts except this one that are not
   removed. A cluster of one host that never recovered has nothing to recover.
2. It asks each of them over its underlay whether it knows a leader (`api.KnowsLeader`,
   `/v1/internal/leader`: a leader answers once a majority has confirmed it, a follower
   names the leader it hears).
   - It refuses if one does, because that cluster is not lost, even when this host reaches
     only a follower of it. Its hosts leave with `host remove`.
   - It also refuses if a host answers without saying, such as with a refused certificate
     (the others may have removed this host) or too late.
   - Only a host that accepts no connection at all (`api.ErrNotReached`) counts as gone.
3. It shows what the remap will do, worked out on a copy of the state, and asks for this
   host's name, which confirms which host goes on alone (`--yes` for scripts). Up to this
   point nothing has changed, and a refusal starts the agent again.
4. For each host, it sets the label `clowder-retired` with the serial of the host's
   certificate (`hetzner.Client.SetRetired`), then powers the server off; Hetzner confirms
   both. The label comes first, because a server that is off but not labeled could come
   back. A server that the leader never checked is checked first.
5. It restarts Raft with this host as the only voter (`raftnode.Recover`). The member opens
   on its own and commits `recover-remap` as the leader. The API forwards no such command.
6. DRBD goes on without the other hosts (`storage.Driver.DropPeers`): `del-peer` for each,
   then quorum off (DRBD refuses that while a peer is configured), then `resume-io` for a
   resource whose writes were suspended. Quorum is off only until the agent adjusts the
   resource to its file, which now names this host alone.
7. It starts the agent. Its loops do the rest: the mesh drops the removed hosts, the
   Floating IP comes here, the placement places what waits, and `forgetPeers` runs
   `forget-peer`.

`recover-remap` (`state/recover.go`) makes every other host `removed` (its certificate
revoked, its ID free once this host has forgotten it) and fits the rest to what this host
holds:

- A volume of replicas 1 or more keeps a synced copy with a disk here. A volume without one
  waits for a restore (`VolumeRestorePending`): the placement gives it no copy, and its
  stack waits with the reason. A tiebreaker of it here is retired. An unsynced copy here
  stays a copy, because the state never removes what may hold older data: `get volumes`
  shows it, and a delete of the volume removes it.
- A volume of replicas 0 whose LV was on another host starts anew, empty, as after a
  failover.
- Every stack that is not on this host, or that names a volume without data, is placed
  anew. Every running operation ends as failed and frees its locks.
- This host, which must be a control-plane host (`clowderd recover` refuses on any other),
  is active and the only voter, and the ingress host if it has that role. `HA` is false
  again, so the cluster grows as a new one does, and `RecoveredAt` raises the alert `no-ha`
  until three control-plane hosts are in service.

A retired host's agent ends at its start (`checkRetired`,
[The agent](host.md#the-agent-internalagent)), because the label names its certificate. A
server that is set up anew gets a certificate of its own and starts, label or not. If
Hetzner's API does not answer at the start, a retired host starts as before
([Known limits](../operating.md#known-limits)), so recover tells the user to set such
servers up anew or delete them.
