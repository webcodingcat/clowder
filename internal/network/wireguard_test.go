package network

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/state"
)

// Host 0's interface to host 2; the keys are WireGuard's own format, so
// bin/dev test-tools loads the file into a real interface.
func TestTheWireGuardConfigMatchesTheGoldenFile(t *testing.T) {
	peer := Peer{
		ID: 2, PublicKey: "1IAgopuUT1u/IoMGFbDAlhSl4zNAqOaVbXxyXIkT8Uk=", Underlay: netip.MustParseAddr("10.0.0.4"),
	}
	checkGolden(t, "clw-2.golden.conf", RenderWireGuard(testKey, 0, peer))
}

func TestOnlyClowdersInterfacesAndRulesCount(t *testing.T) {
	interfaces, err := peerInterfaces(`[{"ifname":"clw-2"},{"ifname":"clw-02"},{"ifname":"clw-32"},` +
		`{"ifname":"wg0"},{"ifname":"clw-x"},{"ifname":"clw-0"}]`)
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(interfaces)), []state.HostID{0, 2}) {
		t.Errorf("interfaces: %v, %v", interfaces, err)
	}
	// a peer's rule has its priority and its table
	rules, err := peerRules(`[{"priority":0,"table":"local"},{"priority":99,"table":"99"},` +
		`{"priority":102,"table":"102"},{"priority":103,"table":"main"},{"priority":131,"table":"131"},` +
		`{"priority":132,"table":"132"},{"priority":32766,"table":"main"}]`)
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(rules)), []state.HostID{2, 31}) {
		t.Errorf("rules: %v, %v", rules, err)
	}
	if none, err := peerInterfaces(""); err != nil || len(none) != 0 {
		t.Errorf("no interfaces: %v, %v", none, err)
	}
}

// fakeTools stands in for ip, wg and conntrack: it answers each command
// line from answers (empty output otherwise) and records the lines it ran.
type fakeTools struct {
	answers map[string]string
	fails   map[string]error
	ran     []string
	// input is what each command got on its standard input, by line
	input map[string]string
}

func (f *fakeTools) run(_ context.Context, _ time.Duration, input, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.ran = append(f.ran, line)
	if input != "" {
		f.input[line] = input
	}
	return f.answers[line], f.fails[line]
}

// meshHost is host 0 with a WireGuard key and fake tools; both peers'
// underlays route through enp7s0 with an MTU of 1450.
func meshHost(t *testing.T) (Host, *fakeTools) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "wireguard.key")
	if err := os.WriteFile(key, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := &fakeTools{
		answers: map[string]string{
			"ip -j route get 10.0.0.3":   `[{"dev":"enp7s0"}]`,
			"ip -j route get 10.0.0.4":   `[{"dev":"enp7s0"}]`,
			"ip -j link show dev enp7s0": `[{"mtu":1450}]`,
		},
		fails: map[string]error{},
		input: map[string]string{},
	}
	return Host{WireGuardKey: key, Run: tools.run}, tools
}

// testKey is a private key in WireGuard's format, made for the tests.
const testKey = "kEgSL+Da4xD7MbjBGfqHwJ+jyCgDJbAt80NqEHyIi1k="

func TestAFileWithoutAKeyStaysOutOfTheError(t *testing.T) {
	host, tools := meshHost(t)
	if err := os.WriteFile(host.WireGuardKey, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := host.ApplyMesh(context.Background(), Mesh{Self: 0, Peers: []Peer{peer2}})
	if err == nil || strings.Contains(err.Error(), "not a key") ||
		slices.Contains(tools.ran, "wg syncconf clw-2 /dev/stdin") {
		t.Errorf("a broken key file: %v, ran %v", err, tools.ran)
	}
}

var (
	peer1 = Peer{ID: 1, PublicKey: "pub1", Underlay: netip.MustParseAddr("10.0.0.3")}
	peer2 = Peer{ID: 2, PublicKey: "pub2", Underlay: netip.MustParseAddr("10.0.0.4")}
)

func TestANewPeerGetsItsInterfaceRoutesAndRule(t *testing.T) {
	host, tools := meshHost(t)
	if err := host.ApplyMesh(context.Background(), Mesh{Self: 0, Peers: []Peer{peer2}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip -j link show type wireguard",
		"ip -j rule show",
		"ip -j route get 10.0.0.4",
		"ip -j link show dev enp7s0",
		"ip link add clw-2 type wireguard",
		"wg syncconf clw-2 /dev/stdin",
		"ip link set clw-2 mtu 1370 up",
		"ip address replace 10.77.0.1/32 dev clw-2 noprefixroute",
		"ip route replace 10.77.0.3/32 dev clw-2 src 10.77.0.1",
		"ip route replace default dev clw-2 table 102",
		"ip rule add priority 102 fwmark 0x102/0xfff lookup 102",
	}
	if !slices.Equal(tools.ran, want) {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(tools.ran, "\n"), strings.Join(want, "\n"))
	}
	if config := tools.input["wg syncconf clw-2 /dev/stdin"]; config != RenderWireGuard(testKey, 0, peer2) {
		t.Errorf("wg syncconf got:\n%s", config)
	}
}

func TestAKnownPeerKeepsItsRuleAndAFormerPeerGoes(t *testing.T) {
	host, tools := meshHost(t)
	tools.answers["ip -j link show type wireguard"] = `[{"ifname":"clw-1"},{"ifname":"clw-2"}]`
	// peer 2's rule as wanted and one with a mark changed by hand; peer 1's
	tools.answers["ip -j rule show"] = `[{"priority":101,"fwmark":"0x101","fwmask":"0xfff","table":"101"},` +
		`{"priority":102,"fwmark":"0x102","fwmask":"0xfff","table":"102"},` +
		`{"priority":102,"fwmark":"0x5","table":"102"}]`
	if err := host.ApplyMesh(context.Background(), Mesh{Self: 0, Peers: []Peer{peer2}}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"ip link add clw-2 type wireguard", "ip rule add priority 102 fwmark 0x102/0xfff lookup 102",
		"ip rule delete priority 102 fwmark 0x102/0xfff lookup 102",
	} {
		if slices.Contains(tools.ran, line) {
			t.Errorf("ran %q for a peer that has it", line)
		}
	}
	// the connections first: the interface or the rule left behind brings a
	// later pass back to them
	formerPeer := []string{
		"conntrack --delete --mark 0x101/0xfff",
		"ip link delete clw-1",
		"ip rule delete priority 101 fwmark 0x101/0xfff lookup 101",
	}
	if index := slices.Index(tools.ran, formerPeer[0]); index < 0 ||
		!slices.Equal(tools.ran[index:index+len(formerPeer)], formerPeer) {
		t.Errorf("ran:\n%s\nwant for the former peer:\n%s", strings.Join(tools.ran, "\n"), strings.Join(formerPeer, "\n"))
	}
	if !slices.Contains(tools.ran, "ip rule delete priority 102 fwmark 0x5 lookup 102") {
		t.Errorf("the rule changed by hand stays:\n%s", strings.Join(tools.ran, "\n"))
	}
}

func TestAFailingPeerHoldsUpNoOther(t *testing.T) {
	host, tools := meshHost(t)
	tools.fails["ip -j route get 10.0.0.3"] = errors.New("network unreachable")
	// no connection had the mark
	tools.answers["ip -j rule show"] = `[{"priority":105,"fwmark":"0x105","fwmask":"0xfff","table":"105"}]`
	tools.fails["conntrack --delete --mark 0x105/0xfff"] = &command.ExitError{Code: 1}
	err := host.ApplyMesh(context.Background(), Mesh{Self: 0, Peers: []Peer{peer1, peer2}})
	if err == nil || !strings.Contains(err.Error(), "host 1") || strings.Contains(err.Error(), "host 5") {
		t.Errorf("the error: %v", err)
	}
	for _, line := range []string{
		"ip rule add priority 102 fwmark 0x102/0xfff lookup 102",
		"ip rule delete priority 105 fwmark 0x105/0xfff lookup 105",
	} {
		if !slices.Contains(tools.ran, line) {
			t.Errorf("did not run %q after host 1 failed:\n%s", line, strings.Join(tools.ran, "\n"))
		}
	}
}
