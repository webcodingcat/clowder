package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/jointoken"
	"github.com/webcodingcat/clowder/internal/state"
)

// fakeJoins takes every join, or refuses it with refusal.
type fakeJoins struct {
	mu       sync.Mutex
	requests []JoinRequest
	refusal  error
}

func (f *fakeJoins) Join(_ context.Context, request JoinRequest) (JoinAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.refusal != nil {
		return JoinAnswer{}, JoinRefused(f.refusal)
	}
	return JoinAnswer{Host: 2, Certificate: "the certificate", DataKey: "the data key"}, nil
}

// first is the first request the leader got.
func (f *fakeJoins) first() JoinRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[0]
}

// refuse makes every later join a refusal with why.
func (f *fakeJoins) refuse(why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusal = why
}

// openToken puts an open join token into the state of every host, as Raft
// would.
func (c *testCluster) openToken(token jointoken.Token) {
	c.t.Helper()
	for _, cluster := range c.clusters {
		cmd, err := state.NewCommand(state.KindAddJoinToken, time.Now(), state.UserActor("alice"),
			state.AddJoinToken{ID: token.ID, Hash: token.Hash(), TTL: time.Hour})
		if err != nil {
			c.t.Fatal(err)
		}
		cluster.mu.Lock()
		_, err = cluster.state.Apply(cluster.state.Index+1, cmd)
		cluster.mu.Unlock()
		if err != nil {
			c.t.Fatal(err)
		}
	}
}

func TestAJoinTokenIsShownOnceAndTheStateKeepsItsHash(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	var answer JoinTokenAnswer
	if err := client.Act(context.Background(), "/v1/join-tokens", JoinTokenRequest{TTL: "2h"}, &answer); err != nil {
		t.Fatal(err)
	}
	token, err := jointoken.Parse(answer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(answer.Expires); until < time.Hour || until > 2*time.Hour {
		t.Errorf("expires %s", answer.Expires)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		encoded, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		_, secret, _ := strings.Cut(answer.Token, ".")
		if current.JoinTokens[token.ID].Hash != token.Hash() || strings.Contains(string(encoded), secret) {
			t.Errorf("tokens %v", current.JoinTokens)
		}
	})
	if err := client.Act(context.Background(), "/v1/join-tokens", JoinTokenRequest{TTL: "1s"}, &answer); err == nil ||
		!strings.Contains(err.Error(), "a join token lives 1m0s to 24h0m0s") {
		t.Errorf("a token of a second: %v", err)
	}
}

func TestOnlyAnOpenTokenGetsAProofOfTheCA(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	token, err := jointoken.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := FindClusterByToken(ctx, cluster.seed(1), token, testVersion); statusOf(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "it is unknown") {
		t.Errorf("a token that is not open: %v", err)
	}
	cluster.openToken(token)
	found, err := FindClusterByToken(ctx, cluster.seed(1), token, testVersion)
	if err != nil || !found.CA.Equal(cluster.ca.Certificate) {
		t.Fatalf("an open token: %v", err)
	}
	// the same ID with another secret: the agent proves nothing to it
	other, err := jointoken.New()
	if err != nil {
		t.Fatal(err)
	}
	_, otherSecret, _ := strings.Cut(other.String(), ".")
	guess, err := jointoken.Parse(token.ID + "." + otherSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindClusterByToken(ctx, cluster.seed(1), guess, testVersion); err == nil ||
		!strings.Contains(err.Error(), "cannot prove it knows the join token") {
		t.Errorf("a wrong secret: %v", err)
	}
}

func TestAFollowerHandsAJoinToTheLeader(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	token, err := jointoken.New()
	if err != nil {
		t.Fatal(err)
	}
	cluster.openToken(token)
	ctx := context.Background()
	found, err := FindClusterByToken(ctx, cluster.seed(1), token, testVersion)
	if err != nil {
		t.Fatal(err)
	}
	request := JoinRequest{
		Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42",
		WGPublicKey: "wg", PublicKey: make([]byte, 32),
	}
	answer, err := found.Join(ctx, request)
	if err != nil || answer.Host != 2 || answer.DataKey != "the data key" {
		t.Fatalf("answer %+v, %v", answer, err)
	}
	sent := cluster.joins.first()
	// the source is where the request came from, as the follower saw it
	if sent.Token != token.String() || len(sent.Nonce) != 16 || time.Since(sent.Time) > time.Minute || sent.Name != "node2" ||
		sent.Source != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("the leader got %+v", sent)
	}

	// a refusal reaches the joining host as it is
	cluster.joins.refuse(errors.New("server 42 is another server"))
	if _, err := found.Join(ctx, request); statusOf(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "server 42 is another server") {
		t.Errorf("a refused join: %v", err)
	}
}

func TestAJoinRequestIsRecentAndWhole(t *testing.T) {
	now := time.Now()
	whole := JoinRequest{
		Name: "node2", Underlay: netip.MustParseAddr("10.0.0.4"), ServerRef: "42", WGPublicKey: "wg",
		PublicKey: make([]byte, 32), Nonce: make([]byte, 16), Time: now,
	}
	if err := CheckJoinRequest(whole, now); err != nil {
		t.Errorf("a whole request: %v", err)
	}
	for want, change := range map[string]func(*JoinRequest){
		"too old":           func(request *JoinRequest) { request.Time = now.Add(-6 * time.Minute) },
		"has no nonce":      func(request *JoinRequest) { request.Nonce = nil },
		"server ref":        func(request *JoinRequest) { request.ServerRef = "" },
		"public keys":       func(request *JoinRequest) { request.WGPublicKey = "" },
		"underlay address":  func(request *JoinRequest) { request.Underlay = netip.Addr{} },
		"clock is off by m": func(request *JoinRequest) { request.Time = now.Add(6 * time.Minute) },
	} {
		request := whole
		change(&request)
		if err := CheckJoinRequest(request, now); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestARemovedLeaderHandsItsLeadOverFirst(t *testing.T) {
	cluster := newTestCluster(t)
	leader := cluster.clusters[0]
	leader.mu.Lock()
	leader.state.Hosts[1].Phase = state.Active
	leader.mu.Unlock()
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	// host 1 is not the leader: no hand-over
	if _, err := client.Post(context.Background(), "/v1/hosts/1/drain", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Post(context.Background(), "/v1/hosts/0/remove", nil); err != nil {
		t.Fatal(err)
	}
	leader.Read(func(current *state.State) {
		if current.Hosts[0].Phase != state.Removed || leader.handOvers != 1 {
			t.Errorf("host 0 is %s after %d hand-overs", current.Hosts[0].Phase, leader.handOvers)
		}
	})
}

func TestARefusedRemovalMovesNoLead(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	if _, err := client.Post(context.Background(), "/v1/hosts/0/remove", nil); statusOf(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "the last that runs stacks") {
		t.Errorf("removing the last host: %v", err)
	}
	cluster.clusters[0].Read(func(*state.State) {
		if cluster.clusters[0].handOvers != 0 {
			t.Error("the lead moved for a removal that was refused")
		}
	})
}

func TestTheBootstrapKeyProvesNothingOnceTheClusterHasItsUser(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	var offer BootstrapOffer
	request, err := http.NewRequest(http.MethodGet, "https://"+cluster.seed(0)+"/v1/bootstrap", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(VersionHeader, testVersion)
	if err := send(uncheckedClient(), request, &offer); err != nil {
		t.Fatal(err)
	}
	if !offer.Bootstrapped || len(offer.Proof) != 0 {
		t.Errorf("offer %+v", offer)
	}
	if _, err := FindCluster(context.Background(), cluster.seed(0), []byte("join"), testVersion); !errors.Is(err, ErrBootstrapped) {
		t.Errorf("a bootstrap after the user: %v", err)
	}
}
