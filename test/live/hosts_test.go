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
	"maps"
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
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/pki"
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
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// the columns by their header, so a new column does not shift them
	header := strings.Fields(lines[0])
	column := map[string]int{}
	for index, name := range header {
		column[name] = index
	}
	// the last columns may be empty (VERSION, before a host reports), so a
	// row needs only those that the test reads
	needed := 0
	for _, name := range []string{"ID", "NAME", "PHASE", "VOTER"} {
		index, ok := column[name]
		if !ok {
			return nil, fmt.Errorf("host list has no column %s: %q", name, lines[0])
		}
		needed = max(needed, index+1)
	}
	var hosts []clusterHost
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < needed {
			return nil, fmt.Errorf("host list: %q", line)
		}
		id, err := strconv.Atoi(fields[column["ID"]])
		if err != nil {
			return nil, fmt.Errorf("host list: %q", line)
		}
		hosts = append(hosts, clusterHost{
			ID: id, Name: fields[column["NAME"]], Phase: fields[column["PHASE"]], Voter: fields[column["VOTER"]] == "yes",
		})
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

// joinToken makes a join token for the roles (a comma-separated list) with
// the CLI; the line after it, on stderr, says until when it opens.
func joinToken(t *testing.T, ttl time.Duration, roles string) string {
	t.Helper()
	output := mustClowder(t, "host", "join-token", "--ttl", ttl.String(), "--roles", roles)
	token, _, _ := strings.Cut(output, "\n")
	if _, err := jointoken.Parse(token); err != nil {
		t.Fatalf("join-token printed %q: %v", output, err)
	}
	return token
}

// applyHosts has OpenTofu make the first host and the joining hosts, each
// new one with its token and each with its roles in the lab's layout, and
// remove those left out; then it reads the outputs anew. The module takes
// roles only for the hosts it makes, so they go here with the hosts.
func applyHosts(t *testing.T, hosts []string, tokens map[string]string) {
	t.Helper()
	layout := labLayout(t)
	// JSON's null would set the variables to null, not to their defaults
	hosts = append([]string{}, hosts...)
	if tokens == nil {
		tokens = map[string]string{}
	}
	roles := map[string]string{firstHost: layout.firstRoles}
	for _, name := range hosts {
		roles[name] = layout.rolesOf(name)
	}
	// HCL takes JSON's lists and objects as they are
	var variables strings.Builder
	for _, variable := range []struct {
		name  string
		value any
	}{{"hosts", hosts}, {"join_tokens", tokens}, {"roles", roles}} {
		encoded, err := json.Marshal(variable.value)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&variables, "%s = %s\n", variable.name, encoded)
	}
	if err := os.WriteFile(filepath.Join(workDir, hostsFile), []byte(variables.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	state := "-state=" + filepath.Join(workDir, "terraform.tfstate")
	var before map[string]moduleHost // none before the first host
	if _, err := os.Stat(filepath.Join(workDir, "outputs.json")); err == nil {
		before = readOutputs(t).Hosts.Value
	}
	t.Logf("OpenTofu applies the first host and the joining hosts %v", hosts)
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
	if _, err := os.Stat(filepath.Join(workDir, "known_hosts")); errors.Is(err, os.ErrNotExist) {
		return // no host was reached yet, so ssh knows no key
	}
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
		eventually(t, joinDeadline, name+"'s setup done", joined.setupReached(setupMarker))
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

// joinHosts lets the layout's other hosts join, each with a token for its
// roles. The first to join is active without a vote; once three
// control-plane hosts are in, they vote. In the layout three the joined
// hosts are drained, so the stacks of the scenarios start on the first
// host, where they look for them; TestPlacement undrains them for a while.
// Last, the test fetches new credentials, which name every control-plane
// host.
func joinHosts(t *testing.T, layout layout) {
	tokens := map[string]string{}
	var names []string
	for _, joining := range layout.joining {
		tokens[joining.name] = joinToken(t, time.Hour, joining.roles)
		names = append(names, joining.name)
	}
	if len(names) > 0 {
		first := names[0]
		applyHosts(t, names[:1], map[string]string{first: tokens[first]})
		eventually(t, joinDeadline, first+" active without a vote", func() error {
			if found, ok := findHost(clusterHosts(t), first); !ok || found.Phase != "active" || found.Voter {
				return fmt.Errorf("%s is %+v", first, found)
			}
			return nil
		})
		applyHosts(t, names, tokens)
		waitForJoined(t, names...)
	}
	eventually(t, time.Minute, fmt.Sprintf("%d voters", layout.voters), func() error {
		return checkVoters(clusterHosts(t), layout.voters)
	})
	if layout.drainJoined {
		for _, name := range names {
			mustClowder(t, "host", "drain", name)
		}
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, usedTokensFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// each joined host kept its mesh and reaches the first host through the
	// private network; the outputs are those of the last apply, which
	// names every host
	tofu := readOutputs(t)
	wantEndpoints := []string{tofu.PublicAddress.Value}
	for _, name := range names {
		joined := host{address: tofu.Hosts.Value[name].PublicAddress}
		joined.must(t, "test -s /var/lib/clowder/net.json")
		if !reaches(t, joined, "", tofu.PrivateAddress.Value+":"+apiPort) {
			t.Errorf("%s does not reach the first host's API", name)
		}
		if layout.hasRole(name, "control-plane") {
			wantEndpoints = append(wantEndpoints, joined.address)
		}
	}
	// new credentials from the first host, as a user fetches them after the
	// hosts joined: every control-plane host's public address is an
	// endpoint, so a scenario that cuts off the first host, the leader after
	// TestSetup, still reaches the cluster through the others
	first := host{address: tofu.PublicAddress.Value}
	issued := first.must(t, "clowderd credentials 2>/dev/null") // stdout alone
	credentials, err := clientconfig.Parse([]byte(issued))
	if err != nil {
		t.Fatal(err)
	}
	endpoints := credentials.Contexts[0].Endpoints
	if !slices.Equal(slices.Sorted(slices.Values(endpoints)), slices.Sorted(slices.Values(wantEndpoints))) {
		t.Fatalf("the credentials name the endpoints %v, want the control-plane hosts %v", endpoints, wantEndpoints)
	}
	writeCredentials(t, []byte(issued))
	if err := os.WriteFile(filepath.Join(workDir, baselineConfigFile), clusterConfigJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkVoters fails unless as many hosts vote as want: one alone, three in
// a cluster of three to five control-plane hosts.
func checkVoters(hosts []clusterHost, want int) error {
	voting := 0
	for _, host := range hosts {
		if host.Voter {
			voting++
		}
	}
	if voting != want {
		return fmt.Errorf("%d hosts vote, want %d: %+v", voting, want, hosts)
	}
	return nil
}

// TestJoin checks who a join refuses: an unknown, used or expired token, a
// server of another and a server that a host has.
func TestJoin(t *testing.T) {
	_, tofu := baseline(t, threeHosts)
	runSteps(t, []step{
		{"an unknown, used or expired token gets no proof", func(t *testing.T) { tokensThatDoNotOpen(t, tofu) }},
		{"a server that is not the host's is refused", func(t *testing.T) { serversOfOthers(t, tofu) }},
	})
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
		_, err = api.FindClusterByToken(ctx, tofu.PublicAddress.Value, api.DefaultConnectTimeout, token, buildinfo.Version)
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
	expiring := joinToken(t, time.Minute, allRoles)
	time.Sleep(time.Minute + 5*time.Second)
	refused(expiring, "it expired")
}

// serversOfOthers joins with an open token, naming a server of the project
// that is no host's, and the server of a host.
func serversOfOthers(t *testing.T, tofu outputs) {
	token, err := jointoken.Parse(joinToken(t, 10*time.Minute, allRoles))
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := api.FindClusterByToken(context.Background(), tofu.PublicAddress.Value, api.DefaultConnectTimeout, token, buildinfo.Version)
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
	// any joined host: TestReplace may have replaced either of the first two
	joined := tofu.Hosts.Value[slices.Sorted(maps.Keys(tofu.Hosts.Value))[0]]
	// the test's requests come from the lab's address: a join naming the
	// stranger's server is refused for that, before Hetzner is asked
	joinedID, err := strconv.ParseInt(joined.ServerID, 10, 64)
	if err != nil {
		t.Fatalf("the module's server_id %q: %v", joined.ServerID, err)
	}
	for _, test := range []struct {
		what     string
		serverID int64
		underlay string
		want     string
	}{
		{"a stranger's server", created.Server.ID, "10.0.0.250", "the request comes from " + labAddress},
		// an underlay of its own: the state names the first thing a host has twice
		{"a host's server", joinedID, "10.0.0.251", "has the server " + joined.ServerID + " already"},
	} {
		_, err := cluster.Join(context.Background(), joinRequest(t, test.serverID, test.underlay))
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

// joinRequest is a host named intruder with keys of its own and every
// role, as the tokens of TestJoin allow.
func joinRequest(t *testing.T, serverID int64, underlay string) api.JoinRequest {
	t.Helper()
	roles, err := hostrole.Parse(allRoles)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	wireGuardKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return api.JoinRequest{
		Name: "intruder", Underlay: netip.MustParseAddr(underlay), PublicAddress: netip.MustParseAddr("192.0.2.250"),
		ServerID:    serverID,
		Roles:       roles,
		PublicKey:   key.Public().(ed25519.PublicKey),
		WGPublicKey: base64.StdEncoding.EncodeToString(wireGuardKey.PublicKey().Bytes()),
	}
}

// TestReplace replaces a host while a stack runs with a copy of its volume
// on it: a new host joins, the old one leaves, its copy moves to the new
// one, and the stack runs on, its container untouched. Once every host
// forgot the old one as a DRBD peer, the next host takes its ID. A host
// remove that would leave fewer than three is refused.
func TestReplace(t *testing.T) {
	server, _ := baseline(t, threeHosts)
	var removed clusterHost
	var container string
	// replaced is the joined host that gets steady's second copy, kept the
	// other joined host
	var replaced, kept string
	runSteps(t, []step{
		{"a stack runs with a copy of its volume on the host to replace", func(t *testing.T) {
			// the joined hosts are drained at the baseline; both take stacks
			// again, because the second copy waits for a third active host
			// to take its tiebreaker
			undrainJoined(t)
			createAndApply(t, "steady", volumeDocument("steady", "fast", failoverVolumeSize, 2)+simpleStack("steady", "steady"))
			waitRunning(t, "steady", 1)
			moveTo(t, "steady", firstHost)
			volumeSteady(t, scenarioHosts(server, readOutputs(t)), "steady")
			for _, volumeCopy := range volumeStatus(t, "steady").Copies {
				if volumeCopy.Host != firstHost && !volumeCopy.Tiebreaker {
					replaced = volumeCopy.Host
				}
			}
			kept = otherThan([]string{firstJoining, secondJoining}, replaced)
			t.Logf("steady has its second copy on %s, which is replaced", replaced)
			writeProof(t, server, "steady", "before")
			container = stackContainer(t, server, "steady")
		}},
		{"a fourth host joins as a host that does not vote", func(t *testing.T) {
			applyHosts(t, []string{firstJoining, secondJoining, thirdJoining},
				map[string]string{thirdJoining: joinToken(t, time.Hour, allRoles)})
			waitForJoined(t, thirdJoining)
			if found, _ := findHost(clusterHosts(t), thirdJoining); found.Voter {
				t.Errorf("the fourth host votes: %+v", found)
			}
		}},
		{"the old host leaves, its copy moves, and no host below three", func(t *testing.T) {
			removed, _ = findHost(clusterHosts(t), replaced)
			mustClowder(t, "host", "remove", replaced, "--yes")
			if output, err := clowder("host", "remove", thirdJoining, "--yes"); err == nil ||
				!strings.Contains(output, "join a replacement first") {
				t.Errorf("a remove below three: %v\n%s", err, output)
			}
			// the copy goes to one of the two other hosts, the tiebreaker to
			// the other
			eventually(t, placementDeadline, "steady's copy off "+replaced, func() error {
				for _, volumeCopy := range volumeStatus(t, "steady").Copies {
					if volumeCopy.Host != firstHost && !volumeCopy.Tiebreaker && volumeCopy.Host != replaced {
						return nil
					}
				}
				return fmt.Errorf("no second copy on %s or %s", kept, thirdJoining)
			})
			hosts := scenarioHosts(server, readOutputs(t))
			delete(hosts, replaced)
			volumeSteady(t, hosts, "steady")
			eventually(t, placementDeadline, replaced+" removed and three voters", func() error {
				hosts := clusterHosts(t)
				if gone, _ := findHost(hosts, replaced); gone.Phase != "removed" {
					return fmt.Errorf("%s is %s", replaced, gone.Phase)
				}
				return checkVoters(hosts, 3)
			})
		}},
		{"the replacement takes the old host's ID", func(t *testing.T) {
			applyHosts(t, []string{kept, thirdJoining, replacement},
				map[string]string{replacement: joinToken(t, time.Hour, allRoles)})
			waitForJoined(t, replacement)
			if found, _ := findHost(clusterHosts(t), replacement); found.ID != removed.ID {
				t.Errorf("the replacement has ID %d, not the removed host's %d", found.ID, removed.ID)
			}
		}},
		{"the stack ran on through it all, with its data", func(t *testing.T) {
			if now := stackContainer(t, server, "steady"); now != container {
				t.Errorf("steady's container changed: %s, then %s", container, now)
			}
			if listed := stackInfo(t, "steady"); listed.Status != "running" || listed.Host != firstHost {
				t.Errorf("steady is %s on %s (%s)", listed.Status, listed.Host, listed.WaitReason)
			}
			checkProof(t, server, "steady", "before")
			// a write that hung without quorum would end here
			server.must(t, "timeout 60 docker exec "+container+" sh -c 'echo after > /data/proof && sync'")
		}},
		{"the hosts are as the baseline wants them again", func(t *testing.T) {
			removeStack(t, "steady")
			drainJoined(t)
		}},
	})
}
