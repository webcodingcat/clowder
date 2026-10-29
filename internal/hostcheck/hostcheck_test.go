package hostcheck

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// fakeHost answers from maps; anything missing is an error.
type fakeHost struct {
	files     map[string]string
	commands  map[string]string
	addresses []netip.Addr
}

func (f fakeHost) ReadFile(path string) ([]byte, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	return []byte(content), nil
}

func (f fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	output, ok := f.commands[strings.Join(append([]string{name}, args...), " ")]
	if !ok {
		return "", errors.New(name + ": exit status 1")
	}
	return output, nil
}

func (f fakeHost) Addresses() ([]netip.Addr, error) {
	return f.addresses, nil
}

// goodHost is a host with the operator base as DEVELOPING.md describes it:
// every check passes.
func goodHost() fakeHost {
	return fakeHost{
		files: map[string]string{
			"/proc/sys/net/ipv4/ip_forward":              "1\n",
			"/proc/sys/net/ipv4/conf/all/rp_filter":      "2\n",
			"/proc/sys/net/ipv4/conf/all/src_valid_mark": "1\n",
			"/proc/sys/kernel/osrelease":                 "6.12.113+deb13-amd64\n",
			"/sys/class/watchdog/watchdog0/identity":     "iTCO_wdt\n",
			"/etc/drbd.d/global_common.conf":             "global {\n\tusage-count no;\n}\n",
			"/etc/docker/daemon.json": `{"live-restore": true, "userland-proxy": false,
				"default-address-pools": [{"base": "172.20.0.0/14", "size": 24}],
				"log-driver": "json-file", "log-opts": {"max-size": "10m", "max-file": "3"}}`,
		},
		commands: map[string]string{
			"timedatectl show --property=NTPSynchronized --value":   "yes\n",
			"systemctl show --property=RuntimeWatchdogUSec --value": "1min\n",
			"modinfo -F name wireguard":                             "wireguard\n",
			"modinfo -F version drbd":                               "9.3.4\n",
			"modinfo -F vermagic drbd":                              "6.12.113+deb13-amd64 SMP preempt mod_unload modversions\n",
			"drbdadm --version":                                     "DRBDADM_BUILDTAG=x\nDRBDADM_VERSION=9.35.0\n",
			"systemctl is-enabled drbd.service":                     "disabled\n",
			"lvmconfig devices/global_filter":                       `global_filter="r|^/dev/drbd.*|"` + "\n",
			"lvs --noheadings --options lv_attr vg-bulk/thin":       "  twi-a-tz--\n",
			"docker compose version --short":                        "2.32.4\n",
		},
		addresses: []netip.Addr{netip.MustParseAddr("198.51.100.11"), netip.MustParseAddr("203.0.113.11")},
	}
}

var goodOptions = Options{Pools: []string{"vg-bulk/thin"}, FloatingIP: netip.MustParseAddr("203.0.113.11")}

func TestAWellPreparedHostPasses(t *testing.T) {
	results := Run(context.Background(), goodHost(), goodOptions)
	for _, result := range results {
		if result.Status != OK {
			t.Errorf("%s: %s %s", result.Name, result.Status, result.Detail)
		}
	}
	if Failed(results) {
		t.Error("Failed reports a failure")
	}
}

func TestEachMissingPieceFailsItsCheck(t *testing.T) {
	cases := map[string]struct {
		change func(host *fakeHost)
		check  string
		detail string
	}{
		"no forwarding": {
			func(h *fakeHost) { h.files["/proc/sys/net/ipv4/ip_forward"] = "0" }, "ip forwarding", "is 0",
		},
		"strict rp_filter": {
			func(h *fakeHost) { h.files["/proc/sys/net/ipv4/conf/all/rp_filter"] = "1" }, "reverse path filter", "is 1",
		},
		"clock not synced": {
			func(h *fakeHost) { h.commands["timedatectl show --property=NTPSynchronized --value"] = "no" },
			"time sync", "not synchronized",
		},
		"watchdog unused": {
			func(h *fakeHost) { h.commands["systemctl show --property=RuntimeWatchdogUSec --value"] = "0" },
			"watchdog", "RuntimeWatchdogSec",
		},
		"in-tree drbd 8.4": {
			func(h *fakeHost) { h.commands["modinfo -F version drbd"] = "8.4.11" }, "drbd module", "not DRBD 9",
		},
		"drbd for another kernel": {
			func(h *fakeHost) { h.files["/proc/sys/kernel/osrelease"] = "6.12.120+deb13-amd64" },
			"drbd module", "not built for the running kernel",
		},
		"debian's drbd-utils": {
			func(h *fakeHost) { h.commands["drbdadm --version"] = "DRBDADM_VERSION=9.22.0" },
			"drbd-utils", "older than 9.28",
		},
		"drbd autostart": {
			func(h *fakeHost) { h.commands["systemctl is-enabled drbd.service"] = "enabled" },
			"drbd autostart off", "enabled",
		},
		"usage report on": {
			func(h *fakeHost) { h.files["/etc/drbd.d/global_common.conf"] = "global { usage-count yes; }" },
			"drbd usage report off", "usage-count no",
		},
		"no lvm filter": {
			func(h *fakeHost) { delete(h.commands, "lvmconfig devices/global_filter") }, "lvm filter", "global_filter",
		},
		"lvm filter accepts drbd": {
			func(h *fakeHost) {
				h.commands["lvmconfig devices/global_filter"] = `global_filter="a|^/dev/drbd.*|"` + "\n"
			},
			"lvm filter", "global_filter",
		},
		"pool missing": {
			func(h *fakeHost) { delete(h.commands, "lvs --noheadings --options lv_attr vg-bulk/thin") },
			"thin pools", "vg-bulk/thin",
		},
		"docker without live-restore and pools": {
			func(h *fakeHost) { h.files["/etc/docker/daemon.json"] = `{"userland-proxy": false}` },
			"docker daemon.json", "live-restore",
		},
		"docker pool overlaps clowder": {
			func(h *fakeHost) {
				h.files["/etc/docker/daemon.json"] = `{"live-restore": true, "userland-proxy": false,
					"default-address-pools": [{"base": "10.76.0.0/14", "size": 24}], "log-opts": {"max-size": "10m"}}`
			},
			"docker daemon.json", "overlaps",
		},
		"unattended upgrades for everything": {
			func(h *fakeHost) {
				h.commands["dpkg-query --show --showformat=${db:Status-Status} unattended-upgrades"] = "installed"
				h.commands["apt-config dump Unattended-Upgrade::Package-Blacklist"] = `"linux-";`
			},
			"unattended upgrades", "docker, drbd, clowder",
		},
		"floating ip missing": {
			func(h *fakeHost) { h.addresses = h.addresses[:1] }, "floating ip", "on no interface",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			host := goodHost()
			c.change(&host)
			results := Run(context.Background(), host, goodOptions)
			for _, result := range results {
				if result.Name != c.check {
					continue
				}
				if result.Status != Fail || !strings.Contains(result.Detail, c.detail) {
					t.Errorf("want FAIL with %q, got %s %q", c.detail, result.Status, result.Detail)
				}
				if !Failed(results) {
					t.Error("Failed does not see the failure")
				}
				return
			}
			t.Fatalf("no check named %q", c.check)
		})
	}
}

func TestChecksWithoutOptionsWarn(t *testing.T) {
	for _, result := range Run(context.Background(), goodHost(), Options{}) {
		if (result.Name == "thin pools" || result.Name == "floating ip") && result.Status != Warn {
			t.Errorf("%s: %s", result.Name, result.Status)
		}
	}
}

func TestPrintShowsStatusNameAndDetail(t *testing.T) {
	var out bytes.Buffer
	if err := Print(&out, []Result{{Name: "drbd module", Status: OK, Detail: "drbd 9.3.4"}}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "OK   drbd module: drbd 9.3.4\n" {
		t.Errorf("got %q", out.String())
	}
}
