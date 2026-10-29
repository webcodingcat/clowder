package hostcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// The overlay ranges of clowder; Docker's address pools must not overlap
// them.
var clowderRanges = []netip.Prefix{
	netip.MustParsePrefix("10.77.0.0/24"),
	netip.MustParsePrefix("10.78.0.0/16"),
}

// The oldest drbd-utils that LINBIT's drbd-dkms accepts: 9.28.
const (
	minimumDRBDUtilsMajor = 9
	minimumDRBDUtilsMinor = 28
)

var (
	drbdUtilsVersion = regexp.MustCompile(`DRBDADM_VERSION=(\d+)\.(\d+)`)
	usageCountOff    = regexp.MustCompile(`(?m)^\s*usage-count\s+no\s*;`)
	// a reject pattern ("r|...|") that names drbd; an accept pattern does not count
	rejectsDRBD = regexp.MustCompile(`"r\|[^|"]*drbd`)
	// a value in apt-config dump: Unattended-Upgrade::Package-Blacklist:: "drbd";
	quoted = regexp.MustCompile(`"([^"]*)"`)
)

// checks in the order they are printed.
var checks = []check{
	{"ip forwarding", sysctl("/proc/sys/net/ipv4/ip_forward", "1",
		"forwarded traffic (published ports, exports) needs net.ipv4.ip_forward = 1")},
	{"reverse path filter", sysctl("/proc/sys/net/ipv4/conf/all/rp_filter", "2",
		"answers without SNAT arrive with the client's address; needs net.ipv4.conf.all.rp_filter = 2")},
	{"source valid mark", sysctl("/proc/sys/net/ipv4/conf/all/src_valid_mark", "1",
		"the return path is chosen by mark; needs net.ipv4.conf.all.src_valid_mark = 1")},
	{"time sync", checkTimeSync},
	{"watchdog", checkWatchdog},
	{"wireguard module", checkWireGuard},
	{"drbd module", checkDRBDModule},
	{"drbd-utils", checkDRBDUtils},
	{"drbd autostart off", checkDRBDService},
	{"drbd usage report off", checkUsageCount},
	{"lvm filter", checkLVMFilter},
	{"thin pools", checkPools},
	{"docker daemon.json", checkDockerDaemon},
	{"docker compose", checkCompose},
	{"unattended upgrades", checkUnattendedUpgrades},
	{"floating ip", checkFloatingIP},
}

// sysctl checks that a /proc/sys file holds want.
func sysctl(path, want, why string) func(context.Context, Host, Options) (Status, string) {
	return func(_ context.Context, host Host, _ Options) (Status, string) {
		value, err := readTrimmed(host, path)
		if err != nil {
			return Fail, err.Error()
		}
		if value != want {
			return Fail, fmt.Sprintf("is %s; %s", value, why)
		}
		return OK, ""
	}
}

func checkTimeSync(ctx context.Context, host Host, _ Options) (Status, string) {
	synced, err := host.Run(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	if err != nil {
		return Fail, fmt.Sprintf("timedatectl: %v", err)
	}
	if strings.TrimSpace(synced) != "yes" {
		return Fail, "the clock is not synchronized (certificates and timeouts need the right time)"
	}
	return OK, ""
}

func checkWatchdog(ctx context.Context, host Host, _ Options) (Status, string) {
	identity, err := readTrimmed(host, "/sys/class/watchdog/watchdog0/identity")
	if err != nil {
		return Fail, "no watchdog device (load softdog: /etc/modules-load.d/)"
	}
	setting, err := host.Run(ctx, "systemctl", "show", "--property=RuntimeWatchdogUSec", "--value")
	if err != nil {
		return Fail, fmt.Sprintf("systemctl: %v", err)
	}
	if setting = strings.TrimSpace(setting); setting == "0" || setting == "infinity" || setting == "" {
		return Fail, fmt.Sprintf("systemd does not use %s; set RuntimeWatchdogSec=60", identity)
	}
	return OK, fmt.Sprintf("%s, systemd pets it every %s", identity, setting)
}

func checkWireGuard(ctx context.Context, host Host, _ Options) (Status, string) {
	if _, err := host.Run(ctx, "modinfo", "-F", "name", "wireguard"); err != nil {
		return Fail, "the kernel has no wireguard module"
	}
	return OK, ""
}

// checkDRBDModule: DRBD 9, built (by DKMS) for the running kernel.
func checkDRBDModule(ctx context.Context, host Host, _ Options) (Status, string) {
	version, err := host.Run(ctx, "modinfo", "-F", "version", "drbd")
	if err != nil {
		return Fail, "no drbd module (install drbd-dkms from LINBIT)"
	}
	version = strings.TrimSpace(version)
	if !strings.HasPrefix(version, "9.") {
		return Fail, fmt.Sprintf("drbd %s is not DRBD 9 (the kernel's own module); install drbd-dkms", version)
	}
	vermagic, err := host.Run(ctx, "modinfo", "-F", "vermagic", "drbd")
	if err != nil {
		return Fail, fmt.Sprintf("modinfo: %v", err)
	}
	kernel, err := readTrimmed(host, "/proc/sys/kernel/osrelease")
	if err != nil {
		return Fail, err.Error()
	}
	if built := strings.Fields(vermagic); len(built) == 0 || built[0] != kernel {
		return Fail, fmt.Sprintf("drbd %s is not built for the running kernel %s (dkms autoinstall)", version, kernel)
	}
	return OK, "drbd " + version
}

func checkDRBDUtils(ctx context.Context, host Host, _ Options) (Status, string) {
	output, err := host.Run(ctx, "drbdadm", "--version")
	if err != nil {
		return Fail, "drbdadm is missing (install drbd-utils from LINBIT)"
	}
	match := drbdUtilsVersion.FindStringSubmatch(output)
	if match == nil {
		return Fail, "cannot read the version of drbd-utils"
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < minimumDRBDUtilsMajor || major == minimumDRBDUtilsMajor && minor < minimumDRBDUtilsMinor {
		return Fail, fmt.Sprintf("drbd-utils %d.%d is older than %d.%d (Debian's own; use LINBIT's)",
			major, minor, minimumDRBDUtilsMajor, minimumDRBDUtilsMinor)
	}
	return OK, fmt.Sprintf("drbd-utils %d.%d", major, minor)
}

// checkDRBDService: only the agent brings resources up.
func checkDRBDService(ctx context.Context, host Host, _ Options) (Status, string) {
	// is-enabled exits non-zero for a disabled or missing unit; only its
	// output matters
	state, _ := host.Run(ctx, "systemctl", "is-enabled", "drbd.service")
	if strings.TrimSpace(state) == "enabled" {
		return Fail, "drbd.service is enabled; only clowderd may bring resources up"
	}
	return OK, ""
}

// checkUsageCount: drbdadm reports to LINBIT's usage survey unless told not to.
func checkUsageCount(_ context.Context, host Host, _ Options) (Status, string) {
	content, err := host.ReadFile("/etc/drbd.d/global_common.conf")
	if err != nil {
		return Fail, err.Error()
	}
	if !usageCountOff.Match(content) {
		return Fail, "set usage-count no; in /etc/drbd.d/global_common.conf"
	}
	return OK, ""
}

// checkLVMFilter: LVM must not scan DRBD devices (it would see the volume
// groups inside volumes twice).
func checkLVMFilter(ctx context.Context, host Host, _ Options) (Status, string) {
	filter, err := host.Run(ctx, "lvmconfig", "devices/global_filter")
	if err != nil || !rejectsDRBD.MatchString(filter) {
		return Fail, `global_filter in /etc/lvm/lvm.conf must reject /dev/drbd* ("r|^/dev/drbd.*|")`
	}
	return OK, ""
}

func checkPools(ctx context.Context, host Host, options Options) (Status, string) {
	if len(options.Pools) == 0 {
		return Warn, "not checked (no --pool given)"
	}
	var missing []string
	for _, pool := range options.Pools {
		attributes, err := host.Run(ctx, "lvs", "--noheadings", "--options", "lv_attr", pool)
		if err != nil || !strings.HasPrefix(strings.TrimSpace(attributes), "t") {
			missing = append(missing, pool)
		}
	}
	if len(missing) > 0 {
		return Fail, "not a thin pool: " + strings.Join(missing, ", ")
	}
	return OK, strings.Join(options.Pools, ", ")
}

// dockerDaemon is the part of /etc/docker/daemon.json clowder relies on.
type dockerDaemon struct {
	LiveRestore   *bool `json:"live-restore"`
	UserlandProxy *bool `json:"userland-proxy"`
	AddressPools  []struct {
		Base string `json:"base"`
		Size int    `json:"size"`
	} `json:"default-address-pools"`
	LogOptions map[string]string `json:"log-opts"`
}

func checkDockerDaemon(_ context.Context, host Host, _ Options) (Status, string) {
	content, err := host.ReadFile("/etc/docker/daemon.json")
	if err != nil {
		return Fail, err.Error()
	}
	var daemon dockerDaemon
	if err := json.Unmarshal(content, &daemon); err != nil {
		return Fail, fmt.Sprintf("not valid JSON: %v", err)
	}
	var problems []string
	if daemon.LiveRestore == nil || !*daemon.LiveRestore {
		problems = append(problems, `"live-restore": true (containers keep running while the agent or Docker restarts)`)
	}
	if daemon.UserlandProxy == nil || *daemon.UserlandProxy {
		problems = append(problems, `"userland-proxy": false (else containers see the proxy instead of the client)`)
	}
	if len(daemon.AddressPools) == 0 {
		problems = append(problems, `"default-address-pools" (Docker's defaults allow only about 30 networks)`)
	}
	for _, pool := range daemon.AddressPools {
		base, err := netip.ParsePrefix(pool.Base)
		if err != nil {
			problems = append(problems, fmt.Sprintf("address pool %q is not a prefix", pool.Base))
			continue
		}
		for _, used := range clowderRanges {
			if base.Overlaps(used) {
				problems = append(problems, fmt.Sprintf("address pool %s overlaps clowder's %s", base, used))
			}
		}
	}
	if daemon.LogOptions["max-size"] == "" {
		problems = append(problems, `"log-opts": {"max-size": ...} (else logs fill the root file system)`)
	}
	if len(problems) > 0 {
		return Fail, "needs " + strings.Join(problems, "; ")
	}
	return OK, ""
}

func checkCompose(ctx context.Context, host Host, _ Options) (Status, string) {
	version, err := host.Run(ctx, "docker", "compose", "version", "--short")
	if err != nil {
		return Fail, "docker compose is missing (packages docker-cli and docker-compose)"
	}
	return OK, "docker compose " + strings.TrimSpace(version)
}

// checkUnattendedUpgrades: the kernel, Docker, DRBD and clowder are only
// updated after a drain.
func checkUnattendedUpgrades(ctx context.Context, host Host, _ Options) (Status, string) {
	// a removed package whose configuration is left is listed too, so look at
	// its status, not only whether dpkg knows it
	status, err := host.Run(ctx, "dpkg-query", "--show", "--showformat=${db:Status-Status}", "unattended-upgrades")
	if err != nil || strings.TrimSpace(status) != "installed" {
		return OK, "not installed"
	}
	dump, err := host.Run(ctx, "apt-config", "dump", "Unattended-Upgrade::Package-Blacklist")
	if err != nil {
		return Fail, fmt.Sprintf("apt-config: %v", err)
	}
	// unattended-upgrades matches each entry against the start of a package
	// name, so these four keep every kernel, Docker, DRBD and clowder package;
	// a longer entry such as linux-firmware would keep only that one
	entries := map[string]bool{}
	for _, match := range quoted.FindAllStringSubmatch(dump, -1) {
		entries[match[1]] = true
	}
	var missing []string
	for _, pattern := range []string{"linux-", "docker", "drbd", "clowder"} {
		if !entries[pattern] {
			missing = append(missing, pattern)
		}
	}
	if len(missing) > 0 {
		return Fail, "Unattended-Upgrade::Package-Blacklist lacks " + strings.Join(missing, ", ") +
			" (they are updated only after clowder host drain)"
	}
	return OK, ""
}

func checkFloatingIP(_ context.Context, host Host, options Options) (Status, string) {
	if !options.FloatingIP.IsValid() {
		return Warn, "not checked (no --floating-ip given)"
	}
	addresses, err := host.Addresses()
	if err != nil {
		return Fail, err.Error()
	}
	for _, address := range addresses {
		if address == options.FloatingIP {
			return OK, ""
		}
	}
	return Fail, fmt.Sprintf("%s is on no interface (every host needs it: Docker publishes the stacks' ports on it, and Docker binds only to local addresses)", options.FloatingIP)
}
