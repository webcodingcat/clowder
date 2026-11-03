# Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs. HLab
needs `/dev/kvm`. The tests use only its interface: `hlab env` prints the API endpoint,
the token, the lab control and the Docker network of a running lab. That network is one
namespace shared by every tool started in it. It holds `192.0.2.2`, where the hosts reach
those tools and from where the tools reach the hosts. Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
bin/dev live                        # up, run, and down once it passes; about an hour
bin/dev live up                     # package, make the first host, bootstrap, two more join
bin/dev live run                    # every scenario on that cluster
bin/dev live run -run TestIngress   # one scenario; the flags go to go test
bin/dev live down                   # destroy the hosts, stop the lab
bin/dev live release v1.2.3-rc1     # on a checkout of the tag: the release, without the test hook
```

Run them only at the end of a piece of work, on its finished state. Without HLab,
`bin/dev all` checks everything but these tests: lint, the unit tests, the renderers
against the real tools, the packages.

`up`, `run` and `down` split the work so a fix costs only the scenario it touches: after
`up` the host stays, and `run -run <test>` runs a scenario again on it as often as
needed. A lock in the volume (`flock` on `.lock`, held while `bin/dev` runs) refuses a
second `bin/dev live` while one runs, since both would share the lab and the volume.

## What `bin/dev live up` sets up

- It builds the packages and starts the lab `clowder-live` (`test/live/lab.yaml`, with
  ports apart from other labs), unless it runs: `hlab up` would start it afresh.
- In the lab's network it serves the `clowderd` package over HTTP (not with a tag) and
  puts the CLI to test in `/work/cli`: the one from `dist/`, or with a tag the one from
  the release's archive.
- It runs a private registry on `192.0.2.2:5000`, with TLS from a throw-away CA and a
  password.
- It runs a mail server for the alerts on `192.0.2.2:1025`: mailpit, with STARTTLS from
  the registry's certificate, any login, and its API on `:8025`.
- It applies `examples/hetzner` with `admin_cidrs` = `192.0.2.2/32`, a third storage
  class `small` of 10 GB, the published port `udp/9987`, and the test hook: the package's
  URL and SHA-256. With a tag there is no hook: `clowderd_version` names the release.
- It copies OpenTofu's binary to `/work/bin`, for the tests that add and remove hosts.
- It runs `TestSetup`, which bootstraps the cluster and has two more hosts join.

The registry and the mail server run until `bin/dev live down`; `run` refuses to start
without them, since a new registry would not hold the test's image. `up` refuses while a
host is left: `down` removes them first, with the hosts of `/work/hosts.tfvars`.

Everything a run keeps is in the Docker volume `clowder-live`: the OpenTofu state and the
joining hosts' variables (`hosts.tfvars`, with their tokens), a throw-away SSH key and
`known_hosts`, the CLI, the registry's files, the cluster and stack directories. A failed scenario leaves the host and what it made for a look.

## The scenarios

`bin/dev live run` runs the Go tests of `test/live` but `TestSetup`, with `-failfast`. Each
drives the host with the real CLI and SSH, in steps that build on each other; the first
failure ends the scenario. Each step has a deadline (the constants in `live_test.go`). On
a failure the test prints cloud-init's status and the journals of `clowder-setup` and
`clowderd`.

Every scenario but `TestSetup` starts from the same baseline and checks it first
(`baseline`): the first host set up and its agent active, the cluster bootstrapped with a
leader, `cluster.yaml` applied and with the settings `TestSetup` gave it, the first host
active and the joined ones drained, three voters, no stack or volume in any namespace, no
fault in the lab and no dead server (HLab's `/status`). The joined hosts are drained so
the scenarios' stacks start on the first host, where the test looks for them: stacks do
not move between hosts yet. A
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
   first host's API over the private network. The test drains both and keeps their
   tokens for `TestJoin`.

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

1. A fourth host, `clowder3`, joins: active, without a vote (three vote).
2. `clowder2` is removed; removing `clowder3` then is refused, as it would leave two.
3. `clowder4` joins in `clowder2`'s place (OpenTofu removes its server) and takes its ID,
   once every host forgot it as a DRBD peer.

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
   mail server. The heartbeat goes on while the first host's agent is stopped: another
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
   host, and the resolver answers it. The probe runs on the host in the container's
   network namespace (`nsenter --net`), so its packets leave through the container's
   bridge.
2. What no answer of the host can vouch for is dropped by its own rule, whose counter
   grows: the metadata service, from the host itself (`guard_out`) and from the
   container; an address of the private network that no host has; and a packet from the
   container with a source outside its network (an address of TEST-NET-1 added to its
   interface, to the registry). The agent reloads the table every loop interval, which
   zeroes the counters, so each probe gets three tries.
3. Both stacks go.
