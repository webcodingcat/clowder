# Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs. HLab
needs `/dev/kvm`. The tests use only HLab's interface: `hlab env` prints the API endpoint,
the token, the lab control and the Docker network of a running lab. The tools that the
tests start in that network share one network namespace with the address `192.0.2.2`. Set
`HLAB` to HLab's wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
bin/dev live                        # every scenario in up to three labs at once, about 40 min
bin/dev live up                     # lab 1: package, image, first host, cluster, two joins
bin/dev live up --lab 2 --layout roles   # lab 2: the cluster of the layout roles
bin/dev live run                    # every scenario of lab 1's layout on its cluster
bin/dev live run -run TestIngress   # one scenario; the flags go to go test
bin/dev live run --lab 2 -run TestQuorum   # on lab 2, left by a failed bin/dev live
bin/dev live down                   # destroy the hosts, stop every lab
bin/dev live down --lab 2           # only lab 2
bin/dev live image --rebuild        # rebuild the hosts' image, even if it is current
bin/dev live release v1.2.3-rc1     # on a checkout of the tag: the release, no test hook
```

Run them only at the end of a piece of work, on its finished state. Without HLab,
`bin/dev all` checks everything else: lint, unit tests, renderers and packages.

`up`, `run` and `down` are separate, so a fix only costs the scenario it touches: the hosts
stay after `up`, and `run -run <test>` repeats a scenario on them. Without `-run`, `run`
runs the scenarios of the lab's layout. The commands work on
lab 1 unless `--lab <n>` names another one; `down` without `--lab` removes every lab. After
`up <tag>`, `run` refuses a checkout that is not at that tag, because the tests must match
the release on the hosts. While `bin/dev` works on a lab, it holds a lock (`flock` on
`.lock` in the lab's volume), so a second run on the same lab fails.

## Layouts

A lab's cluster has one of three layouts (`test/live/layouts_test.go`):

| Layout | Hosts | For |
|---|---|---|
| `three` (default) | `clowder0` to `clowder2`, each with every role; the joined two drained, so stacks start on the first | most scenarios |
| `roles` | `clowder0` to `clowder2` with `control-plane` only, `clowder3` and `clowder4` with `worker,ingress` | the roles: `TestRoles`, `TestRolesFailover`, `TestRolesRecover` |
| `single` | `clowder0` alone, with every role | a cluster of one host: `TestSingleHost` |

`bin/dev live up --layout <layout>` writes the layout's name into the lab's volume
(`layout`), and `TestSetup` builds the cluster after it. Every scenario's baseline fails in
a lab of another layout. In the layout `roles`, the module opens the API's public port on
the control-plane hosts only, so the test reaches the cluster through those alone, as a
user would.

## Several labs at once

`bin/dev live` runs the scenarios in up to three labs at once, `clowder-live-1` to
`clowder-live-3` (`test/live/clowder-live-<n>.yaml`, each with its own ports). It starts
`LIVE_LABS` labs, or else as many as the Docker host's free memory holds (`LAB_MEMORY_GB`
per lab in `bin/dev`). Each lab is a separate cluster with its own registry, mail server
and package server. It runs a group of the scenarios, and its output lines start with
`[lab <n>]`.

`test/live/scenarios` lists every scenario with its run time and its layout. `bin/dev live`
gives the longest remaining group to the lab with the least time so far, and prints each
lab's groups. Each scenario of the layout `three` is a group of its own; the scenarios of
each other layout form one group, so that their cluster is made once. A lab that gets
groups of two layouts runs `up`, `run` and `down` for each, one after the other. A unit test
(`scenarios_test.go`) checks that the file lists every scenario exactly once, with a known
layout. Measure the times again when one changes a lot (`go test -v` prints them).

The packages and the image are built once. Then each lab comes up, runs its group, and is
removed if it passed. A failed lab keeps its hosts for inspection: use `run --lab <n> -run
<test>` and `down --lab <n>`.

## What `bin/dev live up` sets up in a lab

- It builds the packages and the hosts' image, unless the image is current (see below).
- It starts the lab `clowder-live-<n>` unless it is running, because `hlab up` would
  restart it from scratch.
- In the lab's network, it serves the `clowderd` package over HTTP (not with a tag). It
  puts the CLI under test in `/work/cli`: from `dist/`, or with a tag, from the release.
- It runs a private registry on `192.0.2.2:5000`, with a password and TLS from a throwaway
  CA.
- It runs mailpit for the alerts on `192.0.2.2:1025`, with STARTTLS from the registry's
  certificate, any login accepted, and its API on `:8025`.
- It writes the module's variables for `examples/hetzner` (`live.tfvars`): `admin_cidrs` =
  `192.0.2.2/32`, the storage classes `bulk` at 30 GB (for the 20 GB volume of
  `TestNoFalseFailover`) and `small` at 10 GB, the published port `udp/9987`, and the
  image. It also sets the test hook (the package's URL and SHA-256); with a tag,
  `clowderd_version` names the release instead.
- It writes the layout and copies the OpenTofu binary to `/work/bin`: the tests make every
  host with it, because the module takes a host's roles only together with the host
  (`hosts.tfvars`: the hosts, their join tokens and their roles).
- It runs `TestSetup`, which makes the first host, sets up the cluster and lets the
  layout's other hosts join.

The registry and the mail server run until `bin/dev live down`. `run` refuses to start
without them, because a new registry would not hold the test's image. `up` refuses to run
while hosts are left over; `down` removes them, including those in `/work/hosts.tfvars`.

A run keeps its files in the lab's Docker volume `clowder-live-<n>`: the OpenTofu state,
`hosts.tfvars` (with the join tokens), a throwaway SSH key and `known_hosts`, the CLI, the
registry's files, the cluster and stack directories, and the tag after `up <tag>`. A
failed scenario leaves the hosts and what it created for inspection.

## The hosts' image

The kernel headers, the DRBD module built for them, and Docker are the same on every host
and rarely change. So the hosts start from an image that contains them, and their setup
only installs `clowderd`. This takes about one minute instead of four and a half.

`bin/dev live image` makes the image, and `up` makes it when it is missing or old. The
build runs in a separate lab, `clowder-live-image` (`test/live/clowder-live-image.yaml`):

1. `TestImage` creates a Debian 13 server whose cloud-init runs `setup.sh` with
   `PREPARE_ONLY=yes`. This installs the headers, the operator base and the dependencies
   of `clowderd` (the `Depends` of the package in `dist/`), but not `clowderd` and no
   cluster.
2. The test cleans the server for reuse. It removes cloud-init's instance (and with it the
   SSH host keys and the network configuration), the machine-id, Docker's engine ID, the
   package lists and every file of the setup.
3. The server is powered off and snapshotted. `hlab image export --volume` writes the
   snapshot into the Docker volume `clowder-live-image` (`host.qcow2`), and the lab stops.

The lab's own volume keeps Debian's image for the next build; `hlab reset` clears it. A
failed build leaves its lab running for inspection until the next build or
`bin/dev live down`. The labs load only `debian-13`, the one OS image the hosts need.

The image is rebuilt when it is a week old, because the kernel and DRBD on the mirrors move
on, or when its inputs change: Debian's image (its line in `SHA512SUMS`), `setup.sh` and
its unit, `packaging/clowderd.yaml` and `test/live/image_test.go`. `--rebuild` rebuilds it
right away. The inputs' hash is stored in the volume (`inputs`) and in the label
`clowder-live-image` of each snapshot made from the image.

`up` imports the image with `hlab image import --volume` and passes its ID to the module
(`image`) for every host. Both copies stay inside Docker, because through a file on the
host the 3 GB image takes minutes each way on Docker Desktop. This needs an HLab version
that supports `--volume`.

`up <tag>` always builds the image anew. The setup then reads the dependencies from the APT
repository, so the release's setup runs from scratch once per run.

## The scenarios

`bin/dev live run` runs the Go tests in `test/live` of the lab's layout, except `TestSetup`
and `TestImage`, with `-failfast`. Each test drives the hosts with the real CLI and SSH, in steps that build
on each other; the first failure ends the scenario. Each step has a deadline (the
constants in `live_test.go`). On a failure, the test prints cloud-init's status and the
journals of `clowder-setup` and `clowderd`.

Every scenario except `TestSetup` starts by checking the baseline (`baseline`):

- the lab has the scenario's layout;
- the first host is set up, active, and its agent runs;
- the cluster has a leader and the layout's voters (three, or one in `single`);
- the ClusterConfiguration from `TestSetup` is applied;
- in the layout `three`, the joined hosts are drained, so that stacks start on the first
  host, where the test looks for them; in `roles`, every host is active;
- there is no stack or volume in any namespace;
- there is no fault in the lab and no dead server (HLab's `/status`).

`baseline` also corrects a host's clock that lags after a paused VM (`syncClocks`),
because such a host sees a fresh certificate as not yet valid.

Each scenario restores what it changed (for example, it removes its stacks and drains the
joined hosts again), so the scenarios can run in any order. If one fails, the next
`bin/dev live run` stops on what it left behind. `go test` runs them in file order; the
sections below follow the life of a cluster instead.

Faults come from HLab's lab control (`HLAB_CONTROL`, as `hlab fault` uses it). The tests
never change a host in a way that a real failure would not. The test reads
`clowder get stacks -o json` and `docker inspect`, and reads the values it compares within
a single step.

### `TestSetup` (run by `up`)

1. OpenTofu makes the first host with its roles in the layout, and its setup runs.
2. The test gets the first credentials from root on that host over SSH
   (`clowderd credentials`, `clowder config import -`). They go to `/work/clowder-config`
   through `$CLOWDER_CONFIG`, mode 600, not to the developer's `~/.clowder/config`.
   - It exports the sealing key (`clowderd sealing-key export`) to `/work/sealing-key`
     and its public half (`--public`) to `/work/sealing-key.pub`.
   - It writes the cluster's resources into `/work/configuration`, in apply order: the
     SealedSecret `clowder-system/hetzner` with the token, the ClusterConfiguration from
     the module's output `cluster_configuration`, and the namespace `default` with the
     registry's login as an image pull secret. Every value is sealed (`secret seal`).
   - `clowder diff -f` warns about HLab's `http` API, shows what the new token sees, and
     exits with 1. `apply` without `--yes` asks and applies nothing; with `--yes`, it
     applies. A changed token shows as a changed SealedSecret value; the same token sealed
     again shows no change (exit 0).
   - The host trusts the registry's CA (`/etc/docker/certs.d`). The test pushes its image
     and logs out, so only the namespace's login can pull it.
3. The layout's other hosts join, each with a token from `host join-token` for its roles
   (none in `single`). The test writes the hosts, their tokens and their roles into
   `/work/hosts.tfvars` and runs OpenTofu from `/work/bin`. The first joined host becomes
   active without a vote; once three control-plane hosts are in, they vote. Each joined
   host has its `net.json` and reaches the first host's API over the private network. In
   the layout `three`, the test drains the joined hosts and keeps their tokens for
   `TestJoin`. It fetches new credentials, whose endpoints are exactly the control-plane
   hosts. So a scenario that cuts off the first host (the leader after the setup) still
   reaches the cluster.
4. Each host is a separate server, whether it started from the image or from Debian's
   image: cloud-init ran for a new instance, and no two hosts share an instance ID, a
   machine-id or an SSH host key. The DRBD module matches the running kernel.

### `TestAccess`

1. Credentials with `--name live-auditor` apply and delete a Secret. The first host's
   journal logs the certificate it issued, the journals of the control-plane agents log
   the CLI's requests, and the audit (`/v1/audit`) has both commands, each with the actor
   `cli/live-auditor`.
2. The CLI warns before its certificate ends: with a certificate that ends in ten days
   (`ends on <date>, in 10 days`) and with one that has ended (`ended on <date>`). The
   cluster issues CLI certificates for a year, so the test signs these two with a CA of
   its own; the CLI warns before it connects, and the host then refuses them. The test's
   own certificate gets no warning.

### `TestJoin`

1. An unknown token, a token already used for a join, and a token from the minute after its
   own get no proof of the CA (403, each with the reason).
2. With an open token and its proven CA, the agent refuses a join that names a server
   outside the private network (created through HLab's Cloud API), because the request
   does not come from that server's address. It also refuses a join that names a joined
   host's server. Neither creates a host. Because the test sends from its own address, a
   unit test (`clowderd/daemon/joinleader_test.go`) checks that the server's addresses at Hetzner must
   match the host's.

### `TestReplace`

The stack `steady` (a volume of two replicas) runs on the first host throughout. Both
joined hosts take stacks, because the second copy waits for a third active host to take
its tiebreaker. The joined host that gets the second copy is the one that is replaced.

1. A fourth host, `clowder3`, joins: active, without a vote.
2. The host with the copy is removed. Removing `clowder3` after that is refused, because it
   would leave two hosts. The copy moves to one of the two other hosts, the tiebreaker to
   the other, and then the host is gone.
3. `clowder4` joins in its place (OpenTofu removes its server). It takes the removed host's
   ID once every host has forgotten that ID as a DRBD peer.
4. `steady` ran the whole time in the same container, with its data, and still writes.

### `TestPlacement`

1. A stack with a volume of two replicas runs on the first host. The volume is `degraded`,
   with one copy.
2. After `undrain` of the joined hosts, the volume gets its second copy, which syncs, and a
   tiebreaker on the third host. The copy is secondary and unmounted, and the tiebreaker
   is diskless (`drbdsetup status`).
3. The copy's host has pulled the stack's image before it needs it.
4. `stack move` to the copy's host: the stack runs there once, with the data written on the
   first host.
5. `stack move` to the tiebreaker's host: the tiebreaker gets a disk, the old copy is
   removed, and a new tiebreaker appears.
6. A drain of the stack's host moves the stack to the other copy; the copies stay.
7. A stack with a volume of one replica moves, its copy moves with it, and the old copy is
   removed.

The test logs how long the sync and the move take.

### `TestStacks`

The stack `shop` exists from the first step to the last.

1. A stack runs from the private registry with a replicated and a plain volume, a secret
   file and a secret variable, applied with `clowder apply -f`. The password comes as a
   SealedSecret in the stack's file (`secret seal`), the token as a Secret with
   `stringData` on stdin. A value sealed for `clowder-system/hetzner` and pasted into a
   SealedSecret of `default` is refused. `create secret` refuses to create a Secret twice.
2. Applying the same files again answers `unchanged` and recreates nothing.
3. A dry run of a new namespace with its stack answers like a real apply and creates
   nothing.
4. With the credentials deleted, the CLI reaches nothing. `clowderd credentials
   --revoke-older` on the host gives new ones, and the old certificate is refused.
5. A value sealed offline with `secret seal --cert` (the public half) applies as
   `SealedSecret ...: applied`, and sealed again as `unchanged`; `get secrets` shows it as
   a Secret. `secret rekey --identity` of the stack's files and the cluster's resources
   changes nothing in the cluster (`unchanged`, no container recreated). `secret decrypt`
   reads the password back with the sealing key.
6. `stack restart` recreates the unchanged stack's container, and `get stacks --watch`
   shows it `starting`, then `running`.
7. A stack `shop` in the namespace `other` shares no project, container, network or LV
   with the one in `default`. Its replicated volume outlives it: a new stack that names
   the volume right after the delete finds the data. These are refused: a second stack on
   that volume, deleting the volume while a stack names it, and deleting it without
   `--with-data`. Without a stack, the copy is unmounted and `Secondary`, and the next
   stack finds the data (no `--force`, no new file system).
8. An agent restart leaves its containers running, unchanged.
9. A crashed container (`kill -9` of its process; `docker kill` counts as a manual stop)
   runs again within 2 s by Docker's timestamps. An `unhealthy` container is restarted.
   An update of one service recreates that service once, and no other.
10. After a reboot, the stack comes back with its data, no container starts before the
    agent, and `clowderd check` passes. The test removes the setup's marker first, so the
    setup runs again at that boot, as after its own reboot into a newer kernel. With the
    agent stopped and a volume unmounted, the volume's container cannot start, and nothing
    lands on the root file system (the mount guard).
11. A changed secret recreates only its container.
12. With the agent stopped, `docker compose up` fails on a missing secret file. When the
    agent starts again, it brings the stack back.
13. A volume that fits no pool leaves its stack `pending`, with the reason in `get stacks`,
    and does not touch the running stack.
14. Twenty more stacks run.

### `TestIngress`

The stack `echo` exists from step 2 to the last.

1. The Floating IP comes back after the test moves it off the host through the Cloud API.
2. A zone `example.com` in HLab's DNS points `shop.example.com` at the Floating IP. The
   cluster orders the certificate from HLab's Pebble (HTTP-01). Over HTTPS, the stack
   answers with the test's own address in `X-Forwarded-For`. HTTP redirects to HTTPS.
3. A UDP service on a published port sees the test's own address.
4. A container reaches `https://shop.example.com` through the Floating IP (hairpin).
5. With `acme.renew_before` longer than the certificate's remaining time but shorter than
   its lifetime, exactly one new certificate is issued. Longer than the lifetime, there is
   no renewal, and the alert `renew-before-ignored` stays open until the value is removed.
6. The agent stays under 150 MB, and the agent and Traefik together under 300 MB.
   `TestSetup` checks this while idle, `TestStacks` with 21 stacks.

### `TestAlerts`

1. `clowder alert test` reaches Discord, a webhook (the test receives both) and the mail
   server. The heartbeat continues while the first host's agent is stopped, because
   another host leads (stopped for less than the detection time, or the host would be
   fenced). Filling the pool of `small` beyond 80 % raises `pool-use`, and deleting the
   volume sends its all-clear.
2. `promtool check metrics` accepts `/metrics`, and a Prometheus on the host scrapes it.
3. A Hetzner token that HLab refuses (HLab has one project, so this is the token that sees
   none of the servers): `apply` says that the new token could not be checked and,
   without `--yes`, applies nothing. With `--yes`, the alert `hetzner-token` opens at the
   leader's next call to Hetzner, and the right token ends it.

### `TestPriorities`

1. The stack `store` has db (critical, 900), app (the stack's high, 700, depends on db)
   and cache (low, 200). `system_reserve` leaves the host 100M free besides them. A stack
   at 800 that needs 300M pushes out cache; one that needs 500M also pushes out app; db
   always stays. When these stacks are deleted, both services come back. A stack whose
   second service (priority 100) finds nothing below it starts with that service pushed
   out.
2. After the stacks came and went, the agent's memory stays within the limits of
   `TestIngress`.

### `TestBoundaries`

1. A container of the stack `prober` reaches none of these, although each answers the host
   itself:
   - the API on the node address, the underlay and the public address;
   - Raft;
   - `/metrics`;
   - SSH on the underlay, the public address and the Floating IP;
   - a container of the stack `neighbor` on its port;
   - on every other host, the API and Raft on its node address (through WireGuard), and
     the API and SSH on its underlay.

   The other hosts' public addresses are not probed, because the Cloud firewall closes them
   even to the first host. The container does reach the registry and Traefik on the
   Floating IP :443 (the HTTP hairpin), and the resolver answers it. The probe runs on the
   host in the container's network namespace (`nsenter --net`), so its packets leave
   through the container's bridge.
2. Where no answer could prove the block, the test checks that the counter of the host's
   own drop rule grows:
   - the metadata service, from the host itself (`guard_out`) and from the container;
   - an address of the private network that no host has;
   - a packet from the container with a source outside its network (a TEST-NET-1 address
     added to its interface, sent to the registry);
   - a UDP datagram to another host's WireGuard port on its underlay (`@cluster_hosts`).

   The agent reloads the table every loop interval, which resets the counters, so each
   probe gets three tries.

### `TestFailover`

One host, the leader at the start, is fenced three times in three different ways, so the
steps must run in this order. The joined hosts take stacks, and `keeper` (a volume of two
replicas) and `scratch` (replicas 0) run.

1. The leader's host freezes (`vm pause`, like a hanging kernel) with `keeper` on it. The
   other hosts fence it. Within three minutes, `keeper` runs once on another host, with
   its data, and its volume is primary only there. The host is powered on and is `active`
   again.
2. The same host is cut off from every network (`isolate`) and fenced again, with the same
   checks. The fault is removed once the host is fenced, or it would outlive the power
   cycle.
3. Hetzner's API answers 503 and the host freezes. The host is `failed`, and for two
   minutes nothing fails over, while the alert `fencing-blocked` is open. Then the API
   answers again, the test powers the server off as a user would after checking it, and
   `host fence-confirm` lets `keeper` run elsewhere.
4. That was the third fence within the hour, so the host stays off (`disabled`, alert
   `host-disabled`) until `host enable`. Then it comes back, and both alerts are resolved.
5. Another host is cut off a little longer than the detection takes, then comes back.
   Either it was fenced and powered off, or it was not fenced and did not reboot (its boot
   ID tells which). `keeper` runs once.

### `TestNoFalseFailover`

1. `bulk` (a volume of 20 GB with two replicas) gets its second copy. It resyncs at the
   default rate and is still resyncing after the next two steps.
2. The network between the leader and one host is cut. No host fails, during the detection
   time or after it.
3. Every host pauses for 20 s. None fails, none reboots, and `bulk`'s container stays.
4. The resync ends, with every host active (the test logs how long it took).

### `TestCrashingAgent`

1. A drop-in makes `clowderd` exit immediately on a joined host that runs `crasher`. The
   host is fenced, `crasher` runs elsewhere, and the host is powered on.
2. For two minutes, the host stays `fenced` and does not boot again: no reboot loop.
3. Without the drop-in, the agent runs, and the host is `active` again.

### `TestMoveInterrupted`

1. `stack move` of `mover` (a volume of one replica) starts. A copy appears on the new
   host, and a tiebreaker on the third host, which the old host sees as connected.
2. The new host freezes during the resync. `mover` still writes, because its copy keeps
   DRBD's quorum with the tiebreaker. The move ends, and `mover` runs where it was, once,
   with its data. The copy that the move left behind and the tiebreaker are removed. The
   new host had no stack, so it is not fenced: it stays `failed` until it resumes, then
   becomes `active`.

### `TestIngressFailover`

The joined hosts take stacks. The stack `echo` (as in `TestIngress`, but on
`relay.example.com`, which has no certificate) runs on a joined host, not the ingress host.

1. Through the Floating IP, HTTP and UDP see the test's own address. Traffic goes through
   the ingress host to the stack's host (DNAT for UDP, Traefik to Traefik for HTTP). A
   table of the test's own on the stack's host counts packets that leave it with the
   Floating IP as source: there are none.
2. A container on the third host reaches the stack through the Floating IP. Over UDP, the
   stack sees the third host's node address (the hairpin's SNAT). Over HTTP, the request
   goes through that host's own Traefik.
3. A UDP flow from one socket that never pauses gets answers again after `echo` has moved
   to the third host: the ingress host has forgotten the connection's old target.
4. `host drain` of the first host moves the Floating IP to a joined host, and HTTP and UDP
   answer through it. After `undrain`, the first host does not get it back.
5. The ingress host freezes (it has no stacks, so it is not fenced). The Floating IP moves
   to another host within the failover budget, and HTTP and UDP answer through it. The
   host comes back active but does not get the Floating IP back.
6. After cleanup, the Floating IP returns to the first host, the only one that takes
   stacks.

### `TestQuorum`

1. `shifter` (a volume of two replicas) moves to a joined host, which freezes right away,
   before the placement has settled. The copy that the move gave up became the tiebreaker
   in the same command, so the remaining copy keeps DRBD's quorum. `shifter` fails over
   within the budget, with the data written on the frozen host before it froze.
2. Both joined hosts freeze, with an email channel set up. Without a leader, nothing is
   fenced. The first host mails `no quorum as seen from clowder0` on its own (`no-leader`),
   and the all-clear once the other hosts resume.

### `TestRecover`

1. `keeper` (two replicas, on the first host) and `lost` (one replica, on a joined host)
   run, each with data.
2. The joined hosts freeze, and `clowderd recover --power-off --yes` runs on the first
   host.
3. The joined hosts' servers are off and labeled `clowder-retired`, and the hosts are
   removed. The first host leads alone, holds the Floating IP, and the alert `no-ha` is
   open.
4. `keeper` runs on the first host with its data and writes again. The volume `lost` is
   `restore-pending`, and its stack waits for a restore.
5. Switched on, the retired servers start neither Raft nor a container: their agent exits
   with status 4 and says why.
6. `lost` is removed. New servers with the same names join while `keeper` runs. The
   cluster grows from one host to three voters, `keeper`'s volume gets its second copy
   and tiebreaker, its container stays, and `no-ha` is resolved.

### `TestRoles` (layout `roles`)

1. The control-plane hosts answer on the API's public port, the workers do not, and the
   CLI works through the control plane.
2. A volume of two replicas has its copies with a disk on the two workers and its
   tiebreaker on a control-plane host; its stack runs on a worker.
3. A token for `worker,ingress` makes no control-plane host: a join that claims
   `control-plane` gets 403 before the server is checked.
4. With a worker's own certificate, used on the worker: a route for every host
   (`internal/commit-index`) answers, a route for the control plane (`internal/changes`)
   answers 403, and Raft refuses the certificate in the TLS handshake. A control-plane
   host's certificate passes Raft's handshake.
5. From a control-plane host, inside the private network: a worker's API refuses the CLI's
   certificate in the TLS handshake, the control-plane host's API takes it.
6. `keyed`, which names the Secret `keyed-token`, runs on a worker next to the Secret
   `keyed-other`, which no stack names. Only the control-plane hosts hold the data key.
   With its own certificate, the worker gets `keyed-token` from
   `internal/hosts/self/secrets` with its value, and not `keyed-other`. Not yet run: it
   comes with the final live tests of phase 2b.

### `TestRolesFailover` (layout `roles`)

1. `keeper` (two replicas) runs on a worker, with its copies on both workers.
2. Its worker freezes. `keeper` fails over to the other worker within the budget, with its
   data, and the fenced worker comes back active; the copies are on the workers again.

### `TestRolesRecover` (layout `roles`)

Not yet run: it comes with the final live tests of phase 2b.

1. `keeper` (two replicas) runs on a worker, with its copies on both workers and data.
2. `clowder1` and `clowder2` freeze, and `clowderd recover --power-off --yes` runs on the
   first host. It says that it retired the two, and that both workers stay.
3. The first host leads alone. The two servers are off and labeled `clowder-retired`, the
   hosts are removed, and the alert `no-ha` is open.
4. Both workers are active, without a vote, and have set their old Raft log aside
   (`raft.set-aside-*`).
5. `keeper` runs on the same worker in the same container and writes. Its volume has both
   copies synced and its tiebreaker on the first host. The Floating IP is on a worker.
6. New servers with the old names join as control-plane hosts while `keeper` runs. The
   cluster has three voters again, `keeper`'s container stays, and `no-ha` is resolved.

### `TestSingleHost` (layout `single`)

1. The host leads and votes alone, no alert is open (`no-ha` comes only after
   `clowderd recover`), the credentials name its address alone, and it issues join tokens.
2. `solo` (a volume of two replicas) and `loose` (replicas 0) run. `solo`'s volume is
   `degraded` with its one copy and no tiebreaker, and that raises no alert.
3. HTTPS and UDP through the Floating IP see the test's own address, as in `TestIngress`.
4. An agent restart leaves the containers as they are; the host stays active.
5. After a reboot, the host leads again, and the stacks run with their data.

A split brain has no scenario: with DRBD's quorum, neither a lab fault nor a manual action
on the hosts can cause one. A copy promoted with `--force` without the newer data has no
quorum, and its writes hang. The detection of a split brain, its alert and
`volume resolve-splitbrain` are covered by unit tests (`storage`, `controller`, `api`).
