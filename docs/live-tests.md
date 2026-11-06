# Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs. HLab
needs `/dev/kvm`. The tests use only its interface: `hlab env` prints the API endpoint,
the token, the lab control and the Docker network of a running lab. That network is one
namespace that every tool started in it shares. It holds `192.0.2.2`, where the hosts reach
those tools and from where the tools reach the hosts. Point `HLAB` at HLab's wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
bin/dev live                        # every scenario in up to three labs at once, about 40 min
bin/dev live up                     # package, image, lab 1: the first host, its cluster, two more join
bin/dev live run                    # every scenario on lab 1's cluster
bin/dev live run -run TestIngress   # one scenario; the flags go to go test
bin/dev live run --lab 2 -run TestQuorum   # on lab 2, which a failed bin/dev live left
bin/dev live down                   # destroy the hosts, stop every lab
bin/dev live down --lab 2           # only lab 2
bin/dev live image --rebuild        # make the hosts' image anew, even when current
bin/dev live release v1.2.3-rc1     # on a checkout of the tag: the release, without the test hook
```

Run them only at the end of a piece of work, on its finished state. Without HLab,
`bin/dev all` checks everything except these tests: lint, the unit tests, the renderers
against the real tools, and the packages.

`up`, `run` and `down` split the work, so that a fix costs only the scenario it touches.
After `up`, the hosts stay, and `run -run <test>` runs a scenario on them again as often as
needed. The commands work on lab 1 unless `--lab <n>` names another lab; `down` without
`--lab` removes every lab. After `up <tag>`, `run` refuses a checkout that is not the
tag's, because the tests must match the release on the hosts. A lock in each lab's volume
(`flock` on `.lock`, held while `bin/dev` works on the lab) refuses a second
`bin/dev live` on a lab in use, because both runs would share the lab and the volume.

## Several labs at once

`bin/dev live` runs the scenarios in several labs at once, `clowder-live-1` to
`clowder-live-3` (`test/live/clowder-live-<n>.yaml`, each with its own ports). It starts
as many labs as `LIVE_LABS` says, or else as many as the Docker host's free memory holds
(`LAB_MEMORY_GB` in `bin/dev` per lab), from one to three. Each lab is a separate cluster
with its own registry, mail server and package server, and runs a group of the scenarios.
Its output lines start with `[lab <n>]`.

`test/live/scenarios` lists every scenario with its run time. `bin/dev live` splits the
scenarios by these times: it gives the longest one first to the lab with the least time
so far, and it prints each lab's group. A unit test (`scenarios_test.go`) checks that
every scenario of the package is in the file exactly once, so that none is left out.
Measure the times again when a scenario's time changes a lot (`go test -v` prints them).

The packages and the image are built once. Then every lab comes up, runs its group, and is
removed if it passed. A failed lab keeps its hosts for inspection:
`run --lab <n> -run <test>` runs a scenario there again, and `down --lab <n>` removes it.

## What `bin/dev live up` sets up in a lab

- It builds the packages and makes the hosts' image unless the image is current (below).
- It starts the lab `clowder-live-<n>` unless it is running, because `hlab up` would start
  it afresh.
- In the lab's network, it serves the `clowderd` package over HTTP (not with a tag) and
  puts the CLI under test in `/work/cli`: the CLI from `dist/`, or, with a tag, the one
  from the release's archive.
- It runs a private registry on `192.0.2.2:5000`, with TLS from a throwaway CA and a
  password.
- It runs a mail server for the alerts on `192.0.2.2:1025`: mailpit, with STARTTLS from
  the registry's certificate, any login accepted, and its API on `:8025`.
- It applies `examples/hetzner` with `admin_cidrs` = `192.0.2.2/32`, the storage class
  `bulk` at 30 GB (for the 20 GB volume of `TestNoFalseFailover`), a third storage class
  `small` of 10 GB, the published port `udp/9987`, and the test hook: the package's URL
  and SHA-256. With a tag, there is no hook, and `clowderd_version` names the release.
  The hosts start from the image, which is imported into the lab.
- It copies OpenTofu's binary to `/work/bin`, for the tests that add and remove hosts.
- It runs `TestSetup`, which sets up the cluster and has two more hosts join.

The registry and the mail server run until `bin/dev live down`. `run` refuses to start
without them, because a new registry would not hold the test's image. `up` refuses to run
while a host is left over: `down` removes them first, together with the hosts of
`/work/hosts.tfvars`.

Everything that a run keeps is in the lab's Docker volume `clowder-live-<n>`: the OpenTofu
state and the joining hosts' variables (`hosts.tfvars`, with their tokens), a throwaway SSH
key and `known_hosts`, the CLI, the registry's files, the cluster and stack directories,
and the release's tag after `up <tag>`. A failed scenario leaves the hosts and what it
created for inspection.

## The hosts' image

Most of a host's setup is the same on every host and rarely changes: the kernel headers,
the DRBD module built for them, and Docker. So the hosts start from an image that contains
these. Their setup only installs `clowderd` and continues from there, which takes about a
minute instead of four and a half.

`bin/dev live image` makes the image, and `up` makes it when it is missing or old. It
works in a separate lab, `clowder-live-image` (`test/live/clowder-live-image.yaml`).
`TestImage` creates a Debian 13 server whose cloud-init runs `setup.sh` with
`PREPARE_ONLY=yes`. This installs the headers, the operator base and the dependencies of
`clowderd` (the `Depends` of the package in `dist/`), but not `clowderd` itself and no
cluster. Then the test cleans the server for reuse: it removes cloud-init's instance (and
with it the SSH host keys and the network), the machine-id, Docker's engine ID, the package
lists and every file of the setup. The server is powered off and snapshotted, and
`hlab image export --volume` writes the snapshot into the Docker volume
`clowder-live-image` (`host.qcow2`). Then the lab stops. Its own volume keeps Debian's
image for the next time, and `hlab reset` clears that volume first. A failed image build
leaves its lab running for inspection until the next image build or `bin/dev live down`.
The labs load only `debian-13`, the one OS image that the hosts need.

The image is made anew when its inputs change or when it is a week old, because the
mirrors' kernel and DRBD move on; `--rebuild` makes it anew right away. The inputs are
Debian's image (its line in Debian's `SHA512SUMS`), `setup.sh` and its unit,
`packaging/clowderd.yaml` and `test/live/image_test.go`. Their hash is stored in the volume
(`inputs`) and in the label `clowder-live-image` of each snapshot made from the image. `up`
imports the image into its lab with `hlab image import --volume` and passes its ID to the
module (`image`), for the first host and every host that joins later. Both copies stay
inside Docker: through a file on the host, the 3 GB image takes minutes each way on Docker
Desktop. `--volume` needs an HLab version that supports it (from its commit "Move images to
and from Docker volumes" on).

`up <tag>` always makes the image anew, from the release. The setup reads the dependencies
from the APT repository, so the release's setup runs from scratch once per run, and the
hosts start from its result.

## The scenarios

`bin/dev live run` runs the Go tests of `test/live` except `TestSetup` and `TestImage`,
with `-failfast`. Each test drives the hosts with the real CLI and SSH, in steps that build
on each other; the first failure ends the scenario. Each step has a deadline (the
constants in `live_test.go`). On a failure, the test prints cloud-init's status and the
journals of `clowder-setup` and `clowderd`.

Every scenario except `TestSetup` starts from the same baseline and checks it first
(`baseline`):

- the first host is set up and its agent is active;
- the cluster has a leader;
- the ClusterConfiguration is applied, with the settings that `TestSetup` gave it;
- the first host is active and the joined hosts are drained;
- there are three voters;
- there is no stack or volume in any namespace;
- there is no fault in the lab and no dead server (HLab's `/status`).

`baseline` also corrects a host's clock that a paused VM left behind (`syncClocks`),
because a host with a lagging clock sees a certificate that was just issued as not yet
valid. The joined hosts are drained so that the scenarios' stacks start on the first host,
where the test looks for them; `TestPlacement` lets them take stacks and drains them
again. At its end, a scenario puts back what it changed, so the next scenario finds the
baseline again. A scenario that failed stops the next `bin/dev live run` with what it left
behind. So the scenarios can run in any order. `go test` runs them in the order of their
files; the sections below follow the life of a cluster instead. Faults come from HLab's lab
control (`HLAB_CONTROL`, as `hlab fault` uses it), never from changes on the host that a
real failure would not make.

The test reads `clowder get stacks -o json` and `docker inspect`. It reads the values that
it compares within a single step.

### `TestSetup` (run by `up`)

1. The setup creates the first host.
2. The test takes the first credentials from root on that host over SSH
   (`clowderd credentials`, `clowder config import -`). They go to `/work/clowder-config`
   through `$CLOWDER_CONFIG`, with mode 600, not to the developer's `~/.clowder/config`.
   The test exports the cluster's sealing key (`clowderd sealing-key export`) to
   `/work/sealing-key`, and its public half (`--public`) to `/work/sealing-key.pub`. It
   writes the cluster's resources into `/work/configuration`, in the order in which they
   apply: the SealedSecret `clowder-system/hetzner` with the token, the
   ClusterConfiguration from the module's outputs (`cluster_configuration`), and the
   namespace `default` with the registry's login as an image pull secret. Each value is
   sealed (`secret seal`), and none is in clear text in the directory. `clowder diff -f`
   warns about HLab's `http` API, shows what the new token sees and exits with 1.
   `apply` without `--yes` asks for confirmation and applies nothing; with `--yes`, it
   applies. A changed token shows up as a changed value of the SealedSecret, and the same
   token sealed anew as no change (exit 0). The host trusts the registry's CA
   (`/etc/docker/certs.d`). The test pushes its image there and then logs out, so only
   the namespace's login can pull the image.
3. Two hosts join, `clowder1` and `clowder2`, each with a token from `host join-token`.
   The test writes the tokens into `/work/hosts.tfvars` and runs OpenTofu itself (the
   binary that `up` copies to `/work/bin`). The tokens allow all three roles, and every
   host has them. The first joined host becomes active without a vote; once the second
   joins, all three vote. Each joined host
   has its `net.json` and reaches the first host's API over the private network. The test
   drains both, keeps their tokens for `TestJoin`, and fetches new credentials from the
   first host. Their endpoints are all three hosts, so a scenario that cuts off the first
   host (the leader after the setup) still reaches the cluster.
4. Each host is a separate server, whether it started from the image or from Debian's
   image: cloud-init ran for a new instance, and no two hosts share an instance ID, a
   machine-id or an SSH host key. The DRBD module matches the running kernel.

### `TestJoin`

1. An unknown token, the token that a host joined with, and a token from the minute after
   its own minute get no proof of the CA (403, each with the reason).
2. With an open token and the CA that it proved, the agent refuses a join that names a
   server of the project outside the private network (a server that the test creates
   through HLab's Cloud API), because the request does not come from that server's
   address. It also refuses a join that names a joined host's server. No host results from
   either. A unit test (`agent/join_test.go`) checks that the server's addresses at Hetzner
   must match the host's, because here a request comes from the test's own address.

### `TestReplace`

The stack `steady` (a volume of two replicas) runs on the first host from the first step to
the last, with a copy on `clowder2`, the host that is replaced.

1. A fourth host, `clowder3`, joins: active, without a vote (three hosts vote).
2. `clowder2` is removed. Removing `clowder3` after that is refused, because it would leave
   two hosts. `clowder2` leaves: its copy moves to `clowder3`, and then the host is
   removed.
3. `clowder4` joins in `clowder2`'s place (OpenTofu removes `clowder2`'s server) and takes
   its ID, once every host has forgotten it as a DRBD peer.
4. `steady` kept running throughout, in the same container, with its data, and still
   writes.
5. The stack is removed, and the joined hosts are drained again.

### `TestPlacement`

1. A stack with a volume of two replicas runs on the first host; the volume is
   `degraded`, with one copy.
2. The joined hosts take stacks again (`undrain`). The volume gets its second copy, which
   syncs (the test logs how long it takes), and a tiebreaker on the third host. The copy is
   secondary and unmounted, and the tiebreaker is diskless (`drbdsetup status`).
3. The copy's host has pulled the stack's image before it needs it.
4. `stack move` to the copy's host: the stack runs there once, with the data written on
   the first host (the test logs how long the move took).
5. `stack move` to the tiebreaker's host: the tiebreaker gets a disk, the old copy is
   removed, and a new tiebreaker appears.
6. A drain of the stack's host moves the stack to the other copy; the copies stay.
7. A stack with a volume of one replica moves: its copy moves along with it, and the old
   copy is removed.
8. The stacks and volumes are removed, and the joined hosts are drained again.

### `TestStacks`

The stack `shop` exists from the first step to the last.

1. A stack runs from the private registry with a replicated and a plain volume, a secret
   file and a secret variable, applied as resources (`clowder apply -f`). The secrets come
   in both ways: the password as a SealedSecret in the stack's file (`secret seal`), and
   the token as a Secret with `stringData` on stdin. A value sealed for
   `clowder-system/hetzner` and pasted into a SealedSecret of `default` is refused.
   `create secret` makes a Secret and refuses to make it a second time.
2. Applying the same files again answers `unchanged` and recreates nothing.
3. A dry run of a new namespace with its stack answers like a real apply and creates
   nothing.
4. Regaining access: with the credentials deleted, the CLI reaches nothing.
   `clowderd credentials --revoke-older` on the host gives new credentials, and the old
   certificate is refused from then on.
5. The sealing key seals offline and rekeys. A value sealed with `secret seal --cert`
   (the public half, without asking the cluster) applies as `SealedSecret ...: applied`,
   and sealed anew as `unchanged`. `get secrets` shows it as a Secret. `secret rekey
   --identity` of the stack's files and of the cluster's resources changes nothing that
   the cluster keeps (`unchanged`, no container recreated), and `secret decrypt` reads
   the password back with the sealing key.
6. `stack restart` recreates the unchanged stack's container, and `get stacks --watch`,
   running meanwhile, shows it `starting`, then `running`.
7. Namespaces: a stack `shop` in the namespace `other` shares no project, container,
   network or LV with the one in `default`. Its replicated volume outlives it: the stack is
   deleted, and right after that a new stack that names the volume finds the data. A second
   stack on that volume, deleting the volume while a stack names it, and deleting it
   without `--with-data` are refused. Without a stack, the copy is unmounted and
   `Secondary`, and the next stack finds the data again (no `--force`, no new file system).
8. An agent restart leaves its containers running, unchanged.
9. The supervisor: a crashed container (`kill -9` of its process; `docker kill` would be a
   manual stop) runs again within 2 s by Docker's own timestamps. An `unhealthy` container
   is restarted. An update of one service recreates that service, once, and no other.
10. After a reboot, the stack comes back with its data, no container starts before the
    agent, and `clowderd check` passes. The setup's marker is removed beforehand, so the
    setup runs again at that boot, as it does after its own reboot into a newer kernel.
    With the agent stopped and a volume unmounted, the volume's container cannot start,
    and nothing lands on the root file system (the mount guard).
11. A changed secret recreates only its container.
12. With the agent stopped, `docker compose up` fails on a missing secret file; once the
    agent starts again, it brings the stack back.
13. A volume that fits no pool leaves its stack `pending` with the reason in `get stacks`,
    and leaves the running stack untouched.
14. Twenty more stacks run.
15. The stack is removed, then its volumes.

### `TestIngress`

The stack `echo` exists from step 2 to the last.

1. The Floating IP, moved off the host by hand through the Cloud API, comes back.
2. HTTPS: a zone `example.com` in HLab's DNS points `shop.example.com` at the Floating IP.
   The cluster orders its certificate from HLab's Pebble (HTTP-01), and the stack behind it
   answers with the test's own address in `X-Forwarded-For`. HTTP redirects to HTTPS.
3. A UDP service on a published port sees the test's own address.
4. A container reaches `https://shop.example.com` through the Floating IP (hairpin).
5. Renewal: the test sets `acme.renew_before` longer than the certificate's remaining time
   but shorter than its lifetime, and exactly one new certificate is issued. Then it sets
   the value longer than the lifetime: there is no renewal, and the alert
   `renew-before-ignored` stays open until the value is removed.
6. The test logs the memory of the agent and of Traefik. The agent must stay under 150 MB,
   and the agent and Traefik together under 300 MB. `TestSetup` checks this while idle,
   `TestStacks` with 21 stacks.
7. The stack is removed, and `acme` is removed from the ClusterConfiguration.

### `TestAlerts`

1. `clowder alert test` reaches Discord and a webhook (both received by the test) and the
   mail server. The heartbeat continues while the first host's agent is stopped (for less
   than the detection time, or the host would be fenced): another host leads and sends the
   heartbeat. Filling the pool of `small` beyond 80 % raises `pool-use`, and deleting the
   volume brings its all-clear. At the end, the step removes its channels and the pool's
   reserve.
2. `promtool check metrics` accepts `/metrics`, and a Prometheus on the host scrapes it.

### `TestPriorities`

1. Pushing out: the stack `store` has db (critical, 900), app (the stack's high, 700,
   depending on db) and cache (low, 200). `system_reserve` leaves the host 100M free
   besides them. A stack at 800 that needs 300M pushes out cache; one that needs 500M
   pushes out app as well; db always stays. Once these stacks are deleted, both services
   come back. A stack whose second service (priority 100) finds nothing below it starts
   with that service pushed out. Then `store` is removed.
2. The agent's memory after the stacks came and went stays within the limits of
   `TestIngress`.

### `TestBoundaries`

1. A container of the stack `prober` reaches none of the following, each of which answers
   the host itself:
   - the API on the node address, the underlay and the public address;
   - Raft;
   - `/metrics`;
   - SSH on the underlay, the public address and the Floating IP;
   - a container of the stack `neighbor` on its port;
   - on every other host, the API and Raft on its node address (through WireGuard), and
     the API and SSH on its underlay.

   The other hosts' public addresses are not probed, because the Cloud firewall closes them
   even to the first host itself. The container does reach the registry beyond the host and
   Traefik on the Floating IP :443 (the HTTP hairpin), and the resolver answers it. The
   probe runs on the host in the container's network namespace (`nsenter --net`), so its
   packets leave through the container's bridge.
2. What no answer of the host can prove is dropped by its own rule, and the test checks
   that the rule's counter grows:
   - the metadata service, from the host itself (`guard_out`) and from the container;
   - an address of the private network that no host has;
   - a packet from the container with a source outside its network (an address of
     TEST-NET-1 added to its interface, sent to the registry);
   - a UDP datagram to another host's WireGuard port on its underlay (`@cluster_hosts`).

   The agent reloads the table every loop interval, which resets the counters to zero, so
   each probe gets three tries.
3. Both stacks are removed.

### `TestFailover`

One host, the leader at the start, is fenced three times, each time in a different way, so
the steps run in this order. The joined hosts take stacks, and `keeper` (a volume of two
replicas) and `scratch` (replicas 0) run.

1. The leader's host freezes (`vm pause`, like a hanging kernel) with `keeper` on it. The
   other hosts fence it, and `keeper` runs on another host within three minutes, with its
   data, once, with its volume primary on that host alone. The host is powered on, comes
   back, and is `active` again.
2. The same host is cut off from every network (`isolate`): it is fenced again, with the
   same checks. The fault is removed once the host is fenced, because it would outlive the
   power cycle.
3. Hetzner's API answers 503 and the host freezes. The host is `failed`, and for two
   minutes nothing fails over. Then the API answers again, the test powers the server off
   as a user would after checking it, and `host fence-confirm` lets `keeper` run
   elsewhere. Meanwhile the alert `fencing-blocked` is open.
4. That was the third fence within the hour, so the host stays off (`disabled`), with the
   alert `host-disabled`, until `host enable`. Then it comes back, and both alerts are
   resolved.
5. Another host is cut off for a little longer than the detection takes, and then comes
   back. It was either fenced and powered off, or never fenced and not rebooted (its boot
   ID tells which), and `keeper` runs once.
6. The stacks are removed, and the joined hosts are drained again.

### `TestNoFalseFailover`

1. `bulk` (a volume of 20 GB with two replicas) gets its second copy. The copy resyncs at
   the default resync rate during the next steps and is still resyncing after them.
2. The network between the leader and one host is cut: no host fails within the detection
   time and beyond.
3. Every host pauses for 20 s: none fails, none reboots, and `bulk`'s container stays.
4. The resync ends, with every host active (the test logs how long it took).

### `TestCrashingAgent`

1. A drop-in makes `clowderd` exit immediately on a joined host that runs `crasher`. The
   host is fenced, `crasher` runs elsewhere, and the host is powered on.
2. For two minutes, the host stays `fenced` and does not boot again: there is no reboot
   loop.
3. Without the drop-in, the agent runs, and the host is `active` again.

### `TestMoveInterrupted`

1. `stack move` of `mover` (a volume of one replica) starts: a copy appears on the new
   host, and with it a tiebreaker on the third host, which the old host sees as connected.
2. The new host freezes during the resync. `mover` still writes to its volume, because its
   copy keeps DRBD's quorum with the tiebreaker. The move ends, `mover` runs where it was,
   once, with its data, and the copy that the move left behind is removed, together with
   the tiebreaker. The new host had no stack, so it is not fenced: it stays `failed` until
   it resumes, and is then `active` again.

### `TestIngressFailover`

The joined hosts take stacks. The stack `echo` (as in `TestIngress`, but on the domain
`relay.example.com`, which has no certificate) runs on a joined host, not on the ingress
host.

1. Through the Floating IP, HTTP and UDP see the test's own address, through the ingress
   host to the stack's host (DNAT for UDP, Traefik to Traefik for HTTP). A table of the
   test's own on the stack's host counts the packets that leave it with the Floating IP as
   source: there are none.
2. A container on the third host reaches the stack through the Floating IP. Over UDP, the
   stack sees the third host's node address (the hairpin's SNAT); over HTTP, the request
   goes through that host's own Traefik.
3. A UDP flow from one socket that never pauses gets answers again after `echo` has moved
   to the third host: the ingress host has forgotten the connection's old target.
4. `host drain` of the first host moves the Floating IP to a joined host, and HTTP and UDP
   answer through it. After `undrain`, the first host does not get the Floating IP back.
5. The ingress host freezes (a host without stacks, so it is not fenced). The Floating IP
   moves to another host within the failover budget, and HTTP and UDP answer through it.
   When the host is back and active, it does not get the Floating IP back.
6. `echo` is removed, the joined hosts are drained, and the Floating IP comes back to the
   first host, the only one that takes stacks.

### `TestQuorum`

1. `shifter` (a volume of two replicas) moves to a joined host, which freezes right away,
   before the placement has settled. The copy that a move gives up became the tiebreaker
   in the same command, so the remaining copy keeps DRBD's quorum. `shifter` fails over
   within the budget, with the data written on the frozen host before it froze.
2. Both joined hosts freeze, with an email channel set up. Nothing is fenced, because
   there is no leader. The first host mails `no quorum as seen from clowder0` on its own
   (`no-leader`), and sends the all-clear once the other hosts resume.

### `TestRecover`

1. `keeper` (two replicas, on the first host) and `lost` (one replica, on a joined host)
   run, each with data.
2. The joined hosts freeze, and `clowderd recover --power-off --yes` runs on the first
   host.
3. The joined hosts' servers are off and labeled `clowder-retired`, and the hosts are
   removed. The first host leads alone and holds the Floating IP, and the alert `no-ha` is
   open.
4. `keeper` runs on the first host with its data and writes again. The volume `lost` is
   `restore-pending`, and its stack waits for a restore.
5. The retired servers, when switched on, start neither Raft nor a container: their agent
   exits with status 4 and says why.
6. `lost` is removed. New servers with the same names join while `keeper` runs. The
   one-host cluster grows to three, `keeper`'s volume gets its second copy and its
   tiebreaker, its container stays, and three hosts vote; `no-ha` is resolved. Then
   `keeper` is removed, and the joined hosts are drained again.

A split brain has no scenario: with DRBD's quorum, neither a fault in the lab nor a manual
action on the hosts can cause one. A copy that is promoted (`--force`) apart from the newer
data has no quorum, and its writes hang. Its detection, the alert and
`volume resolve-splitbrain` are covered by unit tests (`storage`, `controller`, `api`).
