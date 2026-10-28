# Spike (phase 0)

Throw-away scripts that try the risky paths of the plan on three HLab hosts before any
clowder code exists. They are removed at the end of phase 0; what they show goes into the
plan and the first golden files.

## Run

Needs Docker with `/dev/kvm` and a checkout of HLab. From Git Bash or Linux:

```bash
export HLAB=/path/to/hlab/bin/hlab
spike/run.sh up        # lab "clowder", 3 × cx23 in nbg1, packages, WireGuard mesh (~15 min)
spike/run.sh net       # published ports over WireGuard without SNAT
spike/run.sh drbd      # quorum, fencing, resync, class move, snapshot, recover
spike/run.sh boot      # restart "no" and the mount guard across a reboot
spike/run.sh watchdog  # softdog reboots a hung host
spike/run.sh down      # destroy the hosts, stop the lab
```

`spike/run.sh` without a step runs `up net drbd boot watchdog`. Each check prints `OK` or
`FAIL` and lands in `spike/results/summary.txt`; `measurements.txt` holds timings and
round-trip times, `ruleset-<host>.txt` the nft table, `DOCKER-USER` and routing of each
host.

## What runs where

| File | Runs on | Does |
|---|---|---|
| `lab.yaml` | host | HLab configuration: lab `clowder` on ports 8481/1081 |
| `infra/main.tf` | host (OpenTofu in the lab's network) | private network, `node0`–`node2`, two volumes each, Floating IP on `node0` |
| `run.sh` | host | the steps and checks; talks to the hosts through `hlab ssh` and to the Cloud API from inside the lab |
| `guest/base.sh` | each host | packages (DRBD 9 from LINBIT's `proxmox-9` dist), sysctls, softdog, `daemon.json`, LVM filter, thin pools `vg-bulk` and `vg-fast` |
| `guest/mesh.sh` | each host | `clowder0`, one WireGuard interface per peer, routes with `src`, a routing table per peer |
| `guest/ingress.sh` | each host | table `inet clowder` and `DOCKER-USER` for published ports |
| `guest/drbd.sh` | each host | resource file, thin LVs, metadata |

## The checks

- **net**: alice (an Internet client) reaches UDP and TCP echo services on `node2` through
  the Floating IP on `node0`, and the container sees alice's own address; no packet with the
  Floating IP as source leaves `node2`; after moving the Floating IP to `node1` it still
  works; a running UDP flow follows a moved target only after `conntrack -D`.
- **drbd**: initial sync with a rate limit (round-trip time measured meanwhile), write load
  with protocol C; `node0` cut off: its writes hang, it is powered off, `node1` promotes with
  `node2` as tiebreaker and the data is intact; `node0` comes back without DRBD running and
  resyncs; `node1` moves its disk from `vg-bulk` to `vg-fast` while it stays primary; a
  snapshot on the secondary with `fsfreeze` on the primary; `node0` and `node2` gone, `node1`
  goes on alone.
- **boot**: a container with `restart: "no"` stays down after a reboot, its unmounted volume
  cannot be used, while a container with `unless-stopped` comes back.
- **watchdog**: stopping PID 1 makes softdog reboot the host.
