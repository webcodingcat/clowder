# Placement (`internal/scheduler`, `internal/controller`)

Part of [DEVELOPING.md](../../DEVELOPING.md): where stacks and volume copies go, and how
the leader writes it down. `scheduler` decides on a copy of the state and does no I/O;
`controller` commits what it decided.

## The plan

`scheduler.Plan` reads the state and returns its decisions: what hosts over capacity push
out, where pending stacks go (highest priority first), which stacks move (`Move`), how the
volumes' copies change (`Copies`), why stacks wait, and which pushed-out service may come
back. A stack or volume that an operation holds is left to that operation.

- A volume is placed with the first stack that names it, on that stack's host, with one
  copy, and must fit its pool there (`Fits`). Without overcommit, the pool's volumes and
  the new one must stay within the pool minus its reserve, including a class's fallbacks.
- Volumes, DRBD minors and room are taken only for a stack that fits.
- Only an active worker that no user removes takes stacks and copies with a disk
  (`takesStacks`, `stackHosts`). A tiebreaker holds no data and no load, so any active
  host takes one (`takesTiebreakers`). A host without the worker role has DRBD for it, but
  no thin pool.

## Copies

`copies.go` (`keepCopies`) makes one change per volume and pass.

- A missing copy goes to the host that takes stacks and has the most free space in the
  pool. A tiebreaker on that host gets a disk. The second copy with a disk waits until
  `minTiebreakerHosts` hosts take tiebreakers, so that it never runs without its third
  vote: with two workers and no third host, a volume keeps one copy.
- A copy beyond `replicas` is removed once the others are synced, the one on a leaving
  host first, but never the one where its stack runs.
- A volume with two copies gets a tiebreaker (diskless) on a host without a copy, the one
  with the fewest, once three hosts take tiebreakers. This holds for any `replicas`: a
  volume of replicas 3 on two workers keeps a tiebreaker until a third worker takes its
  third copy.
- **Quorum while copies change** (`fitTiebreaker`). Two copies without their third vote
  lose DRBD's quorum if one fails (one of two nodes). So the change that gives a volume a
  second copy with a disk, or takes it away, adds or removes the tiebreaker in the same
  record. The copy that a move gives up becomes the tiebreaker on its host, so no node
  leaves the resource: its host detaches the disk and removes the LV, whose pool the
  copy's class still names. A volume of replicas 1 has a tiebreaker while a move gives it
  two copies.
- A copy on a host that is leaving does not count; a copy on a draining host does.
- **The resync limit.** A new copy with a disk starts only while no other copy in the
  cluster syncs, so each copy resyncs at `replication.resyncRate` and no link carries more.
  A move whose copies are not yet synced counts as one resync, even though it syncs the
  copies of all its stack's volumes at once.
- A volume may have one copy more than `replicas` (`checkVolume`): the one that a move or
  a leaving host leaves behind, until `keepCopies` gives it up. A volume of replicas 0 that
  no stack runs with, on a leaving host, starts anew, empty, on another host
  (`leaveThrownAway`).
- **After a failure** (`counts`):
  - a copy on a failed or fencing host counts, because the leader has not decided yet;
  - a copy on a fenced host counts for `CopyWait` after the fence, because the leader
    powers the host on again;
  - a copy on a recovering host counts while it catches up;
  - a copy on a disabled host, or on one fenced for longer, is created anew elsewhere.

  Once the host is back, the surplus copy is removed, the unsynced one first. Only a copy
  on a host that runs (active, draining, recovering) takes the resync limit; a copy on a
  host that is down waits for it.

## Failover

`failover.go`: a stack whose host is fenced or disabled (`state.FailsOver`) gets another
host at once, with `set-stack-status` and no move. The old host is off and stops nothing,
and the state allows such a change of host without an operation (`checkLeaderStatus`),
but only to an active worker.

- The new host must have a synced copy with a disk of each of the stack's volumes of
  replicas 1 or more, because nothing can sync from the host that is down. A volume of
  replicas 0 starts anew there, empty, in a new data generation. Of those hosts, the best
  fit wins, as for any placement.
- A stack that finds no host waits on its old host, with the reason in `get stacks` and
  the alert `stack-waiting` ("is down"). With replicas 1 it waits until the host is back.
- A failed host's stacks stay where they are until it is fenced.

## Room and priorities

- **Room is the one measure.** A host offers what it reported (memory, CPUs) minus
  `systemReserve`. Each service of a stack on it that is `starting` or `running` takes its
  reservations (`resources.reserve`, or `defaults.reservations`), unless it is pushed out.
  Nothing measures what containers really use.
- **Priorities.** A service's effective priority is the lowest of its own priority (or the
  stack's) and those of everything it depends on. Stopping a service stops what depends on
  it, so a running service never depends on a pushed-out one. Categories are resolved with
  the current configuration at every pass (`Priorities.Resolve`), so a changed category
  takes effect without a new apply. Pending stacks are placed by the priority of their
  highest service. Only `dependsOn` within a stack counts.
- **Waiting.** `Stack.WaitingSince` is set with the first wait reason and kept while only
  the reason's text changes. It is in the state, so the alert for a stack that waits too
  long survives a change of leader.

## Placing a stack

`place` tries, in this order:

1. a host that has the stack's data and where all of the stack fits, choosing the one
   with the least memory left;
2. a host where all of it fits once its volumes get copies there (a move, within the
   resync limit);
3. for a stack without a host, room made by moving stacks of lower priority to hosts that
   have their data (`makeRoom`; not a stack moved within `MoveCooldown`);
4. last, a host where it fits by pushing out (`fitStack`).

The same order picks the new host of a stack that moves: one that a user asked to move
(`Stack.MoveTo`), or one on a draining host. A stack moves with a move-stack operation
([Moving a stack](state.md#moving-a-stack)). While it moves, its room counts on the new
host, and on the old host until it has stopped there (`stillOn`). A move to the host the
stack already runs on cancels a user's request. A request to a host that no longer takes
stacks is dropped with that host's phase change.

**Pushing out** works like this:

- The stack's services go one by one: highest effective priority first, and at equal
  priority, the services that others depend on first.
- Each takes free room, or pushes out services of any stack below its own priority, lowest
  first, each together with what depends on it.
- A service that still finds no room starts pushed out, together with what depends on it
  within the stack. The stack waits only if its first service finds no room.

An example. A host offers 1 GiB and runs `blog`: one service at priority 500 that reserves
600 MiB. A new stack `shop` arrives: `api` at 800 and `cache` at 200, each reserving
300 MiB.

1. `api` goes first, because it has the higher priority. It takes 300 of the 400 MiB free.
2. `cache` needs 300 MiB, and 100 are left. It may push out only services below its own
   200, and `blog` is at 500. So `cache` starts pushed out, and `blog` keeps running.

A service is only as strong as its own priority: `api` at 800 does not let `cache` push
out `blog`. The new stack's list of pushed-out services is written anew at every
placement, so a list left by an earlier leader is replaced.

**Over capacity** (after a raised `systemReserve`, or a stack whose new generation
reserves more): services below the host's highest effective priority are pushed out,
lowest first, until the rest fits. Services at that highest priority are never pushed out
for this. If that is not enough, nothing is pushed out, and the leader raises the alert
`over-capacity`.

**Coming back.** A pushed-out service whose dependencies all run comes back once its
reservations plus 5 % fit its host's free room for 60 s.

- The highest effective priority goes first, but a service that does not fit does not
  block a smaller one.
- There is one service per decision, because its return changes the room, and only in a
  pass that places and pushes out nothing. So a waiting stack gets room before a
  pushed-out service does.
- The 60 s are kept in the leader's memory and start again after a change of leader.

## Committing the plan

`controller.RunPlacement` runs on every agent but acts only on the leader. It plans on a
copy of the state (`SharedCopy`), outside the lock that Raft's `Apply` waits for.

1. It first writes the push-outs of hosts over capacity. If one fails, it places nothing
   in that pass, because the placements count on the room it frees.
2. For each placement it writes what the stack pushes out (`set-pushed-out`, one command
   for every stack it touches), then the volumes (`put-volume`), then the stack's host and
   `starting` (`set-stack-status`). If the leader changes in between, the services stay
   pushed out without the new stack and come back by the rule above. The stack's host
   does the rest.
3. After the placements it starts the moves (`set-pushed-out` on the new host first, then
   `start-operation`) and writes the copies' changes (`put-volume`).

Each command names the state the pass planned with (`PlannedAt`, starting at the copy's
index; [Planned on a state](state.md#planned-on-a-state)). A command of the pass that
directly follows the planned state (index + 1) becomes part of it, so the next command may
change what that one changed. When something else came in between, the state refuses a
command about what that changed (`ErrStale`, logged as info), and the next pass plans
again.

`controller.RunMoves` runs the leader's steps of each move and watches their deadlines;
`agent/moves.go` records a host's steps once its loops have done them.

## On the host

A pushed-out service is left out of `compose up` (`--scale <service>=0`, which also
removes its running container), and the supervisor's rule for it is "no restart". Only the
leader changes `Stack.PushedOut`; a host's status report does not carry it. A new
generation drops services it no longer has. Whether compose up leaves the rest running is
checked by the live tests, because `bin/dev test-tools` has no Docker daemon.
