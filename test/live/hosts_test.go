//go:build live

package live

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/pki"
)

// The hosts that join the first one (clowder0, the module's default name):
// TestSetup makes two, TestReplace a third and its replacement.
const (
	firstJoining  = "clowder1"
	secondJoining = "clowder2"
	thirdJoining  = "clowder3"
	replacement   = "clowder4"
)

// joinDeadline is how long a joining host may take: the setup as on the
// first host, then the join.
const joinDeadline = setupDeadline

// hostsFile is the module's variables for the joining hosts, which the
// test writes; bin/dev live down hands it to OpenTofu with live.tfvars.
const hostsFile = "hosts.tfvars"

// usedTokensFile keeps the tokens the hosts joined with, for TestJoin.
const usedTokensFile = "used-join-tokens.json"

// clusterHost is a row of clowder host list.
type clusterHost struct {
	ID    int
	Name  string
	Phase string
	Voter bool
}

// clusterHosts reads clowder host list.
func clusterHosts(t *testing.T) []clusterHost {
	t.Helper()
	hosts, err := listHosts()
	if err != nil {
		t.Fatal(err)
	}
	return hosts
}

// listHosts reads clowder host list; while the lead changes, it may fail.
func listHosts() ([]clusterHost, error) {
	output, err := clowder("host", "list")
	if err != nil {
		return nil, err
	}
	var hosts []clusterHost
	for index, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if index == 0 || len(fields) < 4 {
			continue // the header
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("host list: %q", line)
		}
		hosts = append(hosts, clusterHost{ID: id, Name: fields[1], Phase: fields[2], Voter: fields[3] == "yes"})
	}
	return hosts, nil
}

// findHost is the host of that name in the cluster.
func findHost(hosts []clusterHost, name string) (clusterHost, bool) {
	index := slices.IndexFunc(hosts, func(host clusterHost) bool { return host.Name == name })
	if index < 0 {
		return clusterHost{}, false
	}
	return hosts[index], true
}

// joinToken makes a join token with the CLI; the line after it, on stderr,
// says until when it opens.
func joinToken(t *testing.T, ttl time.Duration) string {
	t.Helper()
	output := mustClowder(t, "host", "join-token", "--ttl", ttl.String())
	token, _, _ := strings.Cut(output, "\n")
	if _, err := jointoken.Parse(token); err != nil {
		t.Fatalf("join-token printed %q: %v", output, err)
	}
	return token
}

// applyHosts has OpenTofu make the joining hosts, each new one with its
// token, and remove those left out; then it reads the outputs anew.
func applyHosts(t *testing.T, hosts []string, tokens map[string]string) {
	t.Helper()
	encodedHosts, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	// HCL takes JSON's lists and objects as they are
	variables := fmt.Sprintf("hosts       = %s\njoin_tokens = %s\n", encodedHosts, encodedTokens)
	if err := os.WriteFile(filepath.Join(workDir, hostsFile), []byte(variables), 0o600); err != nil {
		t.Fatal(err)
	}
	state := "-state=" + filepath.Join(workDir, "terraform.tfstate")
	before := readOutputs(t).Hosts.Value
	t.Logf("OpenTofu applies the joining hosts %v (new ones: %d)", hosts, len(tokens))
	tofu(t, "apply", "-input=false", "-auto-approve", state,
		"-var-file="+filepath.Join(workDir, "live.tfvars"), "-var-file="+filepath.Join(workDir, hostsFile))
	if err := os.WriteFile(filepath.Join(workDir, "outputs.json"), []byte(tofu(t, "output", "-json", state)), 0o600); err != nil {
		t.Fatal(err)
	}
	forgetHostKeys(t, before, readOutputs(t).Hosts.Value)
}

// forgetHostKeys drops the SSH host keys of the addresses that a new
// server took: the lab gives a new server the address of one that went,
// and ssh would refuse the new server's key as a changed one.
func forgetHostKeys(t *testing.T, before, after map[string]moduleHost) {
	t.Helper()
	known := map[string]string{} // public address -> server ID
	for _, server := range before {
		known[server.PublicAddress] = server.ServerID
	}
	for name, server := range after {
		if id, ok := known[server.PublicAddress]; ok && id == server.ServerID {
			continue
		}
		command := exec.Command("ssh-keygen", "-f", filepath.Join(workDir, "known_hosts"), "-R", server.PublicAddress)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("forget the SSH host key of %s (%s): %v\n%s", name, server.PublicAddress, err, output)
		}
	}
}

// tofu runs OpenTofu on examples/hetzner, as bin/dev live does it: the
// binary and its data in the work directory, the providers in Go's cache
// volume.
func tofu(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(workDir, "bin", "tofu"), append([]string{"-chdir=examples/hetzner"}, args...)...)
	command.Dir = sourceDir()
	command.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(workDir, "tofu"),
		"TF_PLUGIN_CACHE_DIR=/go/tofu-plugins", "TF_IN_AUTOMATION=1")
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("tofu %s: %v\n%s", args[0], err, exitError.Stderr)
		}
		t.Fatalf("tofu %s: %v", args[0], err)
	}
	return string(output)
}

// sourceDir is the repository's root: go test runs in the package's
// directory.
func sourceDir() string {
	return filepath.Join("..", "..")
}

// waitForJoined waits until every named host's setup is done and the host
// is active.
func waitForJoined(t *testing.T, names ...string) {
	t.Helper()
	tofu := readOutputs(t)
	for _, name := range names {
		joined := host{address: tofu.Hosts.Value[name].PublicAddress}
		eventually(t, joinDeadline, name+"'s setup done", joined.setupDone)
	}
	eventually(t, 5*time.Minute, strings.Join(names, ", ")+" active", func() error {
		hosts := clusterHosts(t)
		for _, name := range names {
			if found, ok := findHost(hosts, name); !ok || found.Phase != "active" {
				return fmt.Errorf("%s is %+v", name, found)
			}
		}
		return nil
	})
}

// growToThreeHosts joins two hosts with a token each: the second waits
// passive, and with the third all of them are active and vote. The joined
// hosts are drained, so the stacks of the scenarios start on the first host,
// where they look for them; TestPlacement undrains them for a while.
func growToThreeHosts(t *testing.T) {
	tokens := map[string]string{firstJoining: joinToken(t, time.Hour), secondJoining: joinToken(t, time.Hour)}
	applyHosts(t, []string{firstJoining}, map[string]string{firstJoining: tokens[firstJoining]})
	eventually(t, joinDeadline, firstJoining+" passive", func() error {
		if found, ok := findHost(clusterHosts(t), firstJoining); !ok || found.Phase != "passive" || found.Voter {
			return fmt.Errorf("%s is %+v", firstJoining, found)
		}
		return nil
	})
	applyHosts(t, []string{firstJoining, secondJoining}, tokens)
	waitForJoined(t, firstJoining, secondJoining)
	eventually(t, time.Minute, "three voters", func() error { return checkThreeVoters(clusterHosts(t)) })
	for _, name := range []string{firstJoining, secondJoining} {
		mustClowder(t, "host", "drain", name)
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, usedTokensFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// each joined host kept its mesh and reaches the first host through the
	// private network; the outputs are those of the second apply, which
	// names both hosts
	tofu := readOutputs(t)
	for _, name := range []string{firstJoining, secondJoining} {
		joined := host{address: tofu.Hosts.Value[name].PublicAddress}
		joined.must(t, "test -s /var/lib/clowder/net.json")
		if !reaches(t, joined, "", tofu.PrivateAddress.Value+":"+apiPort) {
			t.Errorf("%s does not reach the first host's API", name)
		}
	}
}

// checkThreeVoters fails unless three of the hosts vote, as in every
// cluster of three hosts or more up to five.
func checkThreeVoters(hosts []clusterHost) error {
	voting := 0
	for _, host := range hosts {
		if host.Voter {
			voting++
		}
	}
	if voting != 3 {
		return fmt.Errorf("%d hosts vote, want 3: %+v", voting, hosts)
	}
	return nil
}

// TestJoin checks who a join refuses: an unknown, used or expired token, a
// server of another and a server ref that a host has; and that the
// bootstrap key opens nothing any more.
func TestJoin(t *testing.T) {
	_, tofu := baseline(t)
	runSteps(t, []step{
		{"the bootstrap key proves nothing after the bootstrap", func(t *testing.T) { bootstrapKeyIsSpent(t, tofu) }},
		{"an unknown, used or expired token gets no proof", func(t *testing.T) { tokensThatDoNotOpen(t, tofu) }},
		{"a server that is not the host's is refused", func(t *testing.T) { serversOfOthers(t, tofu) }},
	})
}

// bootstrapKeyIsSpent asks for the bootstrap's offer with the key.
func bootstrapKeyIsSpent(t *testing.T, tofu outputs) {
	_, err := api.FindCluster(context.Background(), tofu.PublicAddress.Value, []byte(tofu.BootstrapKey.Value), buildinfo.Version)
	if !errors.Is(err, api.ErrBootstrapped) {
		t.Errorf("the bootstrap key after the bootstrap: %v", err)
	}
}

// tokensThatDoNotOpen asks for the CA's proof with tokens that do not open.
func tokensThatDoNotOpen(t *testing.T, tofu outputs) {
	ctx := context.Background()
	refused := func(text, want string) {
		t.Helper()
		token, err := jointoken.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		_, err = api.FindClusterByToken(ctx, tofu.PublicAddress.Value, token, buildinfo.Version)
		var statusError *api.StatusError
		if !errors.As(err, &statusError) || statusError.Status != http.StatusForbidden || !strings.Contains(err.Error(), want) {
			t.Errorf("want a refusal with %q, got %v", want, err)
		}
	}
	unknown, err := jointoken.New()
	if err != nil {
		t.Fatal(err)
	}
	refused(unknown.String(), "it is unknown")
	content, err := os.ReadFile(filepath.Join(workDir, usedTokensFile))
	if err != nil {
		t.Fatal(err)
	}
	var used map[string]string
	if err := json.Unmarshal(content, &used); err != nil {
		t.Fatal(err)
	}
	refused(used[firstJoining], "it is unknown, or a host joined with it")
	expiring := joinToken(t, time.Minute)
	time.Sleep(time.Minute + 5*time.Second)
	refused(expiring, "it expired")
}

// serversOfOthers joins with an open token, naming a server of the project
// that is no host's, and the server of a host.
func serversOfOthers(t *testing.T, tofu outputs) {
	token, err := jointoken.Parse(joinToken(t, 10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := api.FindClusterByToken(context.Background(), tofu.PublicAddress.Value, token, buildinfo.Version)
	if err != nil {
		t.Fatal(err)
	}
	// a stranger's server, outside the private network
	var created struct {
		Server struct {
			ID int64 `json:"id"`
		} `json:"server"`
		Action struct {
			ID int64 `json:"id"`
		} `json:"action"`
	}
	cloud(t, http.MethodPost, "/servers", map[string]any{
		// a name of its own per run: one left by a failed run stays in the way
		"name": fmt.Sprintf("stranger-%d", time.Now().Unix()), "server_type": "cpx12", "image": "debian-13", "location": "nbg1",
		"start_after_create": false,
	}, &created)
	t.Cleanup(func() {
		// the API refuses to delete a server while its creation runs
		// (locked)
		eventually(t, 5*time.Minute, "the stranger's server made", func() error {
			var action struct {
				Action struct {
					Status string `json:"status"`
				} `json:"action"`
			}
			cloud(t, http.MethodGet, "/actions/"+strconv.FormatInt(created.Action.ID, 10), nil, &action)
			if action.Action.Status == "running" {
				return errors.New("its creation runs")
			}
			return nil
		})
		cloud(t, http.MethodDelete, "/servers/"+strconv.FormatInt(created.Server.ID, 10), nil, nil)
	})
	joined := tofu.Hosts.Value[firstJoining]
	// the test's requests come from the lab's address: a join naming the
	// stranger's server is refused for that, before the hoster is asked
	for _, test := range []struct{ what, serverRef, underlay, want string }{
		{"a stranger's server", strconv.FormatInt(created.Server.ID, 10), "10.0.0.250", "the request comes from " + labAddress},
		// an underlay of its own: the state names the first thing a host has twice
		{"a host's server", joined.ServerID, "10.0.0.251", "has the server ref " + joined.ServerID + " already"},
	} {
		_, err := cluster.Join(context.Background(), joinRequest(t, test.serverRef, test.underlay))
		var statusError *api.StatusError
		if !errors.As(err, &statusError) || statusError.Status != http.StatusForbidden || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: want a refusal with %q, got %v", test.what, test.want, err)
		}
	}
	for _, found := range clusterHosts(t) {
		if found.Name == "intruder" {
			t.Errorf("the cluster took %+v", found)
		}
	}
}

// joinRequest is a host named intruder with keys of its own.
func joinRequest(t *testing.T, serverRef, underlay string) api.JoinRequest {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return api.JoinRequest{
		Name: "intruder", Underlay: netip.MustParseAddr(underlay), ServerRef: serverRef,
		PublicKey:   key.Public().(ed25519.PublicKey),
		WGPublicKey: base64.StdEncoding.EncodeToString(wireGuardKey.PublicKey().Bytes()),
	}
}

// TestReplace replaces a host: a new one joins, the old one goes, and once
// every host forgot it, the next host takes its ID. A host remove that
// would leave fewer than three is refused.
func TestReplace(t *testing.T) {
	baseline(t)
	var removed clusterHost
	runSteps(t, []step{
		{"a fourth host joins as a host that does not vote", func(t *testing.T) {
			applyHosts(t, []string{firstJoining, secondJoining, thirdJoining},
				map[string]string{thirdJoining: joinToken(t, time.Hour)})
			waitForJoined(t, thirdJoining)
			mustClowder(t, "host", "drain", thirdJoining)
			if found, _ := findHost(clusterHosts(t), thirdJoining); found.Voter {
				t.Errorf("the fourth host votes: %+v", found)
			}
		}},
		{"the old host goes, and no host below three", func(t *testing.T) {
			removed, _ = findHost(clusterHosts(t), secondJoining)
			mustClowder(t, "host", "remove", secondJoining, "--yes")
			if output, err := clowder("host", "remove", thirdJoining, "--yes"); err == nil ||
				!strings.Contains(output, "join a replacement first") {
				t.Errorf("a remove below three: %v\n%s", err, output)
			}
			eventually(t, time.Minute, "three voters", func() error { return checkThreeVoters(clusterHosts(t)) })
		}},
		{"the replacement takes the old host's ID", func(t *testing.T) {
			applyHosts(t, []string{firstJoining, thirdJoining, replacement},
				map[string]string{replacement: joinToken(t, time.Hour)})
			waitForJoined(t, replacement)
			mustClowder(t, "host", "drain", replacement)
			if found, _ := findHost(clusterHosts(t), replacement); found.ID != removed.ID {
				t.Errorf("the replacement has ID %d, not the removed host's %d", found.ID, removed.ID)
			}
		}},
	})
}
