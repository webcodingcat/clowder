# Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs. HLab
needs `/dev/kvm`. The tests use only its interface: `hlab env` prints the API endpoint,
the token, the lab control and the Docker network of a running lab. That network is one
namespace shared by every tool started in it. It holds `192.0.2.2`, where the hosts reach
those tools and from where the tools reach the hosts. Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
bin/dev live                        # every scenario in up to three labs at once, about 40 min
bin/dev live up                     # package, image, lab 1: the first host, bootstrap, two more join
bin/dev live run                    # every scenario on lab 1's cluster
bin/dev live run -run TestIngress   # one scenario; the flags go to go test
bin/dev live run --lab 2 -run TestQuorum   # on lab 2, which a failed bin/dev live left
bin/dev live down                   # destroy the hosts, stop every lab
bin/dev live down --lab 2           # only lab 2
bin/dev live image --rebuild        # make the hosts' image anew, even when current
bin/dev live release v1.2.3-rc1     # on a checkout of the tag: the release, without the test hook
```

Run them only at the end of a piece of work, on its finished state. Without HLab,
`bin/dev all` checks everything but these tests: lint, the unit tests, the renderers
against the real tools, the packages.

`up`, `run` and `down` split the work so a fix costs only the scenario it touches: after
`up` the hosts stay, and `run -run <test>` runs a scenario again on them as often as
needed. They work on lab 1 unless `--lab <n>` names another; `down` without it removes
every lab. After `up <tag>`, `run` refuses a checkout that is not the tag's, as the tests
must match the release on the hosts. A lock in each lab's volume (`flock` on `.lock`,
held while `bin/dev` works on the lab) refuses a second `bin/dev live` on a lab in use,
since both would share the lab and the volume.

## Several labs at once

`bin/dev live` runs the scenarios in several labs at once, `clowder-live-1` to
`clowder-live-3` (`test/live/clowder-live-<n>.yaml`, each with ports of its own): as many
as `LIVE_LABS` says, else as many as the Docker host's free memory holds (`LAB_MEMORY_GB` in
`bin/dev` a lab), from one to three. Each lab is a cluster of its own, with its registry, mail server and
package server, and runs a group of the scenarios; its lines start with `[lab <n>]`.

`test/live/scenarios` lists every scenario with its run time; `bin/dev live` splits them
by it, the longest first to the lab with the least so far, and prints each lab's group.
A unit test (`scenarios_test.go`) checks that every scenario of the package is in the
file once, so none is left out. Measure the times anew when a scenario's time changes much
(`go test -v` prints them).

The packages and the image are made once, then every lab comes up, runs its group and is
removed once it passed. A failed lab keeps its hosts for a look: `run --lab <n> -run
<test>` runs a scenario again there, `down --lab <n>` removes it.

## What `bin/dev live up` sets up in a lab

- It builds the packages and makes the hosts' image unless it is current (below).
- It starts the lab `clowder-live-<n>` unless it runs: `hlab up` would start it afresh.
- In the lab's network it serves the `clowderd` package over HTTP (not with a tag) and
  puts the CLI to test in `/work/cli`: the one from `dist/`, or with a tag the one from
  the release's archive.
- It runs a private registry on `192.0.2.2:5000`, with TLS from a throw-away CA and a
  password.
- It runs a mail server for the alerts on `192.0.2.2:1025`: mailpit, with STARTTLS from
  the registry's certificate, any login, and its API on `:8025`.
- It applies `examples/hetzner` with `admin_cidrs` = `192.0.2.2/32`, the storage class
  `bulk` at 30 GB (for the 20 GB volume of `TestNoFalseFailover`), a third storage class
  `small` of 10 GB, the published port `udp/9987`, and the test hook: the package's
  URL and SHA-256. With a tag there is no hook: `clowderd_version` names the release.
  The hosts start from the image, imported into the lab.
- It copies OpenTofu's binary to `/work/bin`, for the tests that add and remove hosts.
- It runs `TestSetup`, which bootstraps the cluster and has two more hosts join.

The registry and the mail server run until `bin/dev live down`; `run` refuses to start
without them, since a new registry would not hold the test's image. `up` refuses while a
host is left: `down` removes them first, with the hosts of `/work/hosts.tfvars`.

Everything a run keeps is in the lab's Docker volume `clowder-live-<n>`: the OpenTofu
state and the joining hosts' variables (`hosts.tfvars`, with their tokens), a throw-away
SSH key and `known_hosts`, the CLI, the registry's files, the cluster and stack
directories, and the release's tag after `up <tag>`. A failed scenario leaves the hosts
and what it made for a look.

## The hosts' image

Most of a host's setup is the same on every host and changes rarely: the kernel's
headers, DRBD's module built for them, Docker. So the hosts start from an image that has
it, and their setup installs only `clowderd` and goes on from there (about a minute
instead of four and a half).

`bin/dev live image` makes it, and `up` does when it is missing or old. In a lab of its
own, `clowder-live-image` (`test/live/clowder-live-image.yaml`), `TestImage` makes a
server of Debian 13 whose cloud-init runs `setup.sh` with `PREPARE_ONLY=yes`: the
headers, the operator base and what `clowderd` depends on (the `Depends` of the package
of `dist/`), but not `clowderd` and no cluster. Then it cleans the server for reuse:
cloud-init's instance (with it the SSH host keys and the network), the machine-id,
Docker's engine ID, the package lists and every file of the setup go. The server is
powered off and snapshotted, `hlab image export --volume` writes the snapshot into the
Docker volume `clowder-live-image` (`host.qcow2`), and the lab stops; its own volume keeps
Debian's image for the next time, and `hlab reset` clears it before. A failed image
leaves its lab running for a look until the next image or `bin/dev live down`. The labs
load only `debian-13`, the one OS image the hosts need.

The image is made anew when its inputs change or it is a week old, as the mirrors' kernel
and DRBD move on; `--rebuild` makes it anew at once. The inputs are Debian's image (its
line in Debian's `SHA512SUMS`), `setup.sh` and its unit, `packaging/clowderd.yaml` and
`test/live/image_test.go`; their hash is in the volume (`inputs`) and in the label
`clowder-live-image` of each snapshot made from it. `up` imports the image into its lab
with `hlab image import --volume` and gives its ID to the module (`image`), for the first
host and every host that joins later. Both copies stay inside Docker: through a file on
the host, the 3 GB image takes minutes each way on Docker Desktop. `--volume` needs an
HLab that has it (from its commit "Move images to and from Docker volumes" on).

`up <tag>` always makes the image anew, from the release: the setup reads the
dependencies from the APT repository, so the release's setup runs from scratch once per
run, and the hosts start from what it made.

## The scenarios

`bin/dev live run` runs the Go tests of `test/live` but `TestSetup` and `TestImage`, with
`-failfast`. Each drives the host with the real CLI and SSH, in steps that build on each
other; the first failure ends the scenario. Each step has a deadline (the constants in
`live_test.go`). On a failure the test prints cloud-init's status and the journals of
`clowder-setup` and `clowderd`.

Every scenario but `TestSetup` starts from the same baseline and checks it first
(`baseline`): the first host set up and its agent active, the cluster bootstrapped with a
leader, `cluster.yaml` applied and with the settings `TestSetup` gave it, the first host
active and the joined ones drained, three voters, no stack or volume in any namespace, no
fault in the lab and no dead server (HLab's `/status`); a host's clock that a paused VM left
behind it brings back (`syncClocks`), as a lagging clock takes a certificate just issued
as not yet valid. The joined hosts are drained so
the scenarios' stacks start on the first host, where the test looks for them;
`TestPlacement` takes them in and gives them back. A
scenario puts back what it changed at its end, so the next finds the baseline again; one
that failed stops the next `bin/dev live run` with what it left. So the scenarios run in
any order; `go test` runs them in the order of their files, and the sections below follow
the way of a cluster instead. Faults come from HLab's lab control
(`HLAB_CONTROL`, as `hlab fault` does), never from changes on the host that a real
failure would not make.

The test reads `clowder get stacks -o json` and `docker inspect`. Values it compares it
reads within a step.

### `TestSetup` (run by `up`)

1. The setup makes the first host and deletes its key file.
2. The user bootstraps (its credentials go to `/work/config`, mode 600, not to the
   developer's config directory), fills in `cluster.yaml` from the module's outputs and
   applies it, with the hoster token and the registry's password encrypted (`secret
   encrypt --cluster-yaml`). The cluster directory holds `cluster.yaml` alone, with no
   value in clear. `cluster diff` names a changed token, by name only, and nothing for the
   same token encrypted anew. The recovery key goes to `/work/recovery-key`. The host
   trusts the registry's CA (`/etc/docker/certs.d`), and the test pushes its image there,
   then logs out, so only the agent's login pulls it.
3. Two hosts join, `clowder1` and `clowder2`, each with a token of `host join-token`: the
   test writes them into `/work/hosts.tfvars` and runs OpenTofu itself (the binary that
   `up` copies to `/work/bin`). The first waits `passive` without a vote; with the second,
   all three are active and vote. Each joined host has its `net.json` and reaches the
   first host's API over the private network. The test drains both, keeps their
   tokens for `TestJoin` and takes the endpoints of the module's `cluster_yaml`, all
   three hosts, into `cluster.yaml`, so a scenario that cuts off the first host, the leader
   after the setup, still reaches the cluster.
4. Each host is a server of its own, whether it started from the image or from Debian's:
   cloud-init ran for a new instance, and no two hosts share an instance ID, a machine-id
   or an SSH host key. DRBD's module fits the running kernel.

### `TestJoin`

1. The bootstrap key proves nothing any more: the offer has no proof, and the CLI stops
   with "has a user already".
2. An unknown token, the token a host joined with and a token of a minute after its
   minute get no proof of the CA (403, each with why).
3. With an open token and the CA it proved, a join is refused that names a server of the
   project outside the private network (a server the test makes through HLab's Cloud
   API), as it does not come from that server's address, and one that names a joined
   host's server. No host comes of it. That the hoster's addresses must match the
   host's is a unit test (`agent/join_test.go`): a request comes from the test's own
   address here.

### `TestReplace`

The stack `steady` (a volume of two replicas) runs on the first host from the first step to
the last, with a copy on `clowder2`, the host that is replaced.

1. A fourth host, `clowder3`, joins: active, without a vote (three vote).
2. `clowder2` is removed; removing `clowder3` then is refused, as it would leave two.
   `clowder2` leaves: its copy moves to `clowder3`, and then it is removed.
3. `clowder4` joins in `clowder2`'s place (OpenTofu removes its server) and takes its ID,
   once every host forgot it as a DRBD peer.
4. `steady` ran on throughout, in the same container, with its data, and writes.
5. The stack goes, and the joined hosts are drained again.

### `TestPlacement`

1. A stack with a volume of two replicas runs on the first host; the volume is
   `degraded`, with one copy.
2. The joined hosts take stacks again (`undrain`): the volume gets its second copy, which
   syncs (the test logs how long), and a tiebreaker on the third host. The copy is
   secondary and unmounted, the tiebreaker diskless (`drbdsetup status`).
3. The copy's host pulled the stack's image before it needs it.
4. `stack move` to the copy's host: the stack runs there once, with the data written on
   the first host (the test logs how long the move took).
5. `stack move` to the tiebreaker's host: it gets a disk, the old copy goes, and a new
   tiebreaker comes.
6. A drain of the stack's host moves it to the other copy; the copies stay.
7. A stack with a volume of one replica moves: a copy of its own comes along, and the old
   one goes.
8. The stacks and volumes go, and the joined hosts are drained again.

### `TestStacks`

The stack `shop` lives from the first step to the last.

1. A stack runs from the private registry with a replicated and a plain volume, a secret
   file and a secret variable, applied as resources (`clowder apply -f`), each secret
   encrypted with `secret encrypt`: the password in the stack's file, the token on stdin.
   A Secret in clear, in a file and on stdin, and the hoster's token as a Secret are
   refused. `secret rekey` of the stack's files and of `cluster.yaml` changes nothing the
   cluster keeps (`unchanged`, no container recreated), and `secret decrypt` reads a value
   back with the recovery key.
2. The same files applied again answer `unchanged` and recreate nothing.
3. A dry run of a new namespace with its stack answers as a real apply and makes nothing.
4. Access back: with the credentials deleted, the CLI reaches nothing; a code from
   `clowderd user recovery-code` on the host and `user recover` give new credentials; the
   same code a second time and the old certificate are refused.
5. `recovery rotate` with valid credentials but a code for another user is refused; with
   the user's own code it makes a new recovery key. After `secret rekey`, the stack's file
   opens with the new key and no longer with the old one, and the cluster keeps what it had.
   The new key replaces `/work/recovery-key`.
6. `stack restart` recreates the unchanged stack's container, and `get stacks --watch`,
   running meanwhile, shows it `starting`, then `running`.
7. Namespaces: a stack `shop` in the namespace `other` shares no project, container,
   network or LV with default's. Its replicated volume outlives it: the stack is deleted
   and, right after, a new stack that names the volume finds the data. A second stack on
   that volume, a delete of it while a stack names it, and a delete without `--with-data`
   are refused. Without a stack the copy is unmounted and `Secondary`, and the next stack
   finds the data again (no `--force`, no new file system).
8. An agent restart leaves its containers running, unchanged.
9. The supervisor: a crashed container (`kill -9` of its process; `docker kill` would be a
   stop by hand) runs again within 2 s by Docker's own times, an `unhealthy` one is
   restarted, and an update of one service recreates that one, once, and no other.
10. After a reboot the stack comes back with its data, no container started before the
    agent, and `clowderd check` passes. The setup's marker is gone before, so the setup
    runs again at that boot, as after its own reboot into a newer kernel. With the agent
    stopped and a volume unmounted, its container cannot start and nothing lands on the
    root file system (the mount guard).
11. A changed secret recreates its container only.
12. With the agent stopped, `docker compose up` fails on a missing secret file; started
    again, the agent brings the stack back.
13. A volume that fits no pool leaves its stack `pending` with the reason in `get stacks`,
    and the running stack untouched.
14. Twenty more stacks run.
15. The stack goes, then its volumes.

### `TestIngress`

The stack `echo` lives from step 2 to the last.

1. The Floating IP, moved off the host by hand through the Cloud API, comes back.
2. HTTPS: a zone `example.com` in HLab's DNS points `shop.example.com` at the Floating IP;
   the cluster orders its certificate at HLab's Pebble (HTTP-01), and the stack behind it
   answers with the test's own address in `X-Forwarded-For`. HTTP redirects to HTTPS.
3. A UDP service on a published port sees the test's own address.
4. A container reaches `https://shop.example.com` through the Floating IP (hairpin).
5. Renewal: the test sets `acme.renew_before` longer than the certificate's time left but
   shorter than its lifetime. Exactly one new certificate comes. Then it sets the value
   longer than the lifetime: no renewal, and the alert `renew-before-ignored` is open
   until the value is removed.
6. The memory of the agent and Traefik is logged: the agent must stay under 150 MB, the
   agent and Traefik together under 300 MB. `TestSetup` checks it idle, `TestStacks` with
   21 stacks.
7. The stack goes, and `acme` leaves `cluster.yaml`.

### `TestAlerts`

1. `clowder alert test` reaches Discord and a webhook (both received by the test) and the
   mail server. The heartbeat goes on while the first host's agent is stopped (shorter
   than the detection, or it would be fenced): another
   host leads and calls it. Filling the pool of `small` beyond 80 % raises `pool-use`; deleting the volume
   brings its all-clear. The step removes its channels and the pool's reserve at the end.
2. `promtool check metrics` accepts `/metrics`, and a Prometheus on the host scrapes it.

### `TestPriorities`

1. Pushing out: the stack `store` has db (critical, 900), app (the stack's high, 700,
   depending on db) and cache (low, 200). `system_reserve` leaves the host 100M free
   besides them. A stack at 800 that needs 300M pushes out cache; one that needs 500M
   pushes out app too; db always stays. Deleted, they let both come back. A stack whose
   second service (priority 100) finds nothing below it starts with that service pushed
   out. Then `store` goes.
2. The agent's memory after the stacks came and went, within the goals of `TestIngress`.

### `TestBoundaries`

1. A container of the stack `prober` reaches none of these, each of which answers the
   host itself: the API on the node address, the underlay and the public address, Raft,
   `/metrics`, SSH on the underlay, the public address and the Floating IP, a container
   of the stack `neighbor` on its port, and of every other host the API and Raft on its
   node address (through WireGuard) and the API and SSH on its underlay. The other hosts'
   public addresses are not probed: the Cloud firewall closes them to the first host
   itself. It reaches the registry beyond the
   host and Traefik on the Floating IP :443 (the HTTP hairpin), and the resolver answers it.
   The probe runs on the host in the container's
   network namespace (`nsenter --net`), so its packets leave through the container's
   bridge.
2. What no answer of the host can vouch for is dropped by its own rule, whose counter
   grows: the metadata service, from the host itself (`guard_out`) and from the
   container; an address of the private network that no host has; and a packet from the
   container with a source outside its network (an address of TEST-NET-1 added to its
   interface, to the registry); and a UDP datagram to another host's WireGuard port on its
   underlay (`@cluster_hosts`). The agent reloads the table every loop interval, which
   zeroes the counters, so each probe gets three tries.
3. Both stacks go.

### `TestFailover`

One host, the leader at the start, is fenced three times, each time another way, so its
steps run in this order. The joined hosts take stacks, and `keeper` (a volume of two
replicas) and `scratch` (replicas 0) run.

1. The leader's host freezes (`vm pause`, a kernel that hangs) with `keeper` on it: the
   others fence it, and `keeper` runs on another host within three minutes, with its data,
   once, with its volume primary on that host alone. The host is powered on, comes back
   and is `active` again.
2. The same host, cut off from every network (`isolate`): fenced again, the same checks.
   The fault is removed once it is fenced, as it would outlive the power cycle.
3. The hoster's API answers 503 and the host freezes: it is `failed`, and for two minutes
   nothing fails over. Then the API answers again, the test powers the server off as a
   user who checks, and `host fence-confirm` lets `keeper` run elsewhere. Meanwhile the
   alert `fencing-blocked` is open.
4. That was the third fence within the hour: the host stays off (`disabled`), with the
   alert `host-disabled`, until `host enable`; then it comes back, and both alerts are
   resolved.
5. Another host, cut off for a little longer than the detection takes and then back: it
   was either fenced and powered off, or never fenced and not rebooted (its boot ID says
   which), and `keeper` runs once.
6. The stacks go, and the joined hosts are drained again.

### `TestNoFalseFailover`

1. `bulk` (a volume of 20 GB with two replicas) gets its second copy, which resyncs at
   the default resync rate during the next steps, and is still resyncing after them.
2. The network between the leader and one host is cut: no host fails within the
   detection time and more.
3. Every host pauses for 20 s: none fails, none reboots, `bulk`'s container stays.
4. The resync ends, with every host active (the test logs how long it took).

### `TestCrashingAgent`

1. A drop-in makes `clowderd` exit at once on a joined host that runs `crasher`: it is
   fenced, `crasher` runs elsewhere, and the host is powered on.
2. For two minutes it stays `fenced` and does not boot again: no reboot loop.
3. Without the drop-in the agent runs, and the host is `active` again.

### `TestMoveInterrupted`

1. `stack move` of `mover` (a volume of one replica) starts: a copy comes to the new host,
   and with it a tiebreaker on the third, which the old host sees connected.
2. The new host freezes during the resync: `mover` still writes to its volume, as its copy
   keeps DRBD's quorum with the tiebreaker. The move ends, `mover` runs where it was, once,
   with its data, and the copy the move left goes, the tiebreaker with it. The new host had
   no stack, so it is
   not fenced: it stays `failed` until it goes on, and is `active` again.

### `TestIngressFailover`

The joined hosts take stacks; the stack `echo` (as in `TestIngress`, on the domain
`relay.example.com`, which has no certificate) runs on a joined host, not on the ingress
host.

1. Through the Floating IP, HTTP and UDP see the test's own address, through the ingress
   host to the stack's (DNAT for UDP, Traefik to Traefik for HTTP). A table of the test's
   own on the stack's host counts the packets that leave it with the Floating IP as
   source: none.
2. A container on the third host reaches the stack through the Floating IP: over UDP,
   the stack sees the third host's node address (the hairpin's SNAT); over HTTP, through
   that host's own Traefik.
3. A UDP flow that never pauses, from one socket, answers again after `echo` moved to
   the third host: the ingress host forgot the connection's old target.
4. `host drain` of the first host moves the Floating IP to a joined host; HTTP and UDP
   answer through it. Undrained, the first host does not get the Floating IP back.
5. The ingress host freezes (a host without stacks, so it is not fenced): the Floating IP
   goes to another host within the failover budget, and HTTP and UDP answer through it.
   Back and active, the host does not get it back.
6. `echo` goes, the joined hosts are drained, and the Floating IP comes back to the first
   host, the only one that takes stacks.

### `TestQuorum`

1. `shifter` (a volume of two replicas) moves to a joined host, which freezes at once,
   before the placement settled: the copy a move gives up became the tiebreaker in the
   same command, so the copy left keeps DRBD's quorum, and `shifter` fails over within
   the budget, with the data written on the frozen host before it froze.
2. Both joined hosts freeze, with an email channel set up: nothing is fenced, as there is
   no leader, and the first host mails `no quorum as seen from clowder0` by itself
   (`no-leader`), and the all-clear once they go on.

### `TestRecover`

1. `keeper` (two replicas, on the first host) and `lost` (one replica, on a joined host)
   run, each with data.
2. The joined hosts freeze, and `clowderd recover --power-off --yes` runs on the first
   host.
3. The joined hosts' servers are off and labeled `clowder-retired`, the hosts removed; the
   first host leads alone, holds the Floating IP, and the alert `no-ha` is open.
4. `keeper` runs on the first host with its data and writes again; the volume `lost` is
   `restore-pending`, and its stack waits for a restore.
5. The retired servers, switched on, start neither Raft nor a container: their agent ended
   with status 4 and says why.
6. `lost` goes. New servers under the same names join while `keeper` runs: the cluster of
   one host grows to three, `keeper`'s volume gets its second copy and its tiebreaker,
   its container stays, and three hosts vote; `no-ha` is resolved. Then `keeper` goes, and
   the joined hosts are drained again.

A split brain has no scenario: with DRBD's quorum neither a fault of the lab nor a hand on
the hosts makes one. A copy promoted (`--force`) apart from the newer data has no quorum,
and its writes hang. Its detection, the alert and `volume resolve-splitbrain` are unit
tests (`storage`, `controller`, `api`).
