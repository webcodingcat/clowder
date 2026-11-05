package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/command"
	"github.com/webcodingcat/clowder/internal/controller"
	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
)

// recoverTimeout bounds each step of recover that waits on Hetzner or
// Raft: a power action, the new leader, the remap.
const recoverTimeout = 3 * time.Minute

// RecoverParts are what recover changes on the host and asks of others;
// clowderd recover passes the real ones, the tests fakes.
type RecoverParts struct {
	// Storage drops the DRBD peers that are gone (storage.Driver.DropPeers)
	Storage interface {
		DropPeers(ctx context.Context, peers []int) error
	}
	// Service runs systemctl's action ("stop", "start") on clowderd.service
	Service func(ctx context.Context, action string) error
	// KnowsLeader asks the agent at address (another host's underlay and API
	// port), as this host, whether it knows a leader (api.KnowsLeader)
	KnowsLeader func(ctx context.Context, address string, certificate tls.Certificate, ca *x509.Certificate) (bool, error)
}

// RecoverOptions say how recover confirms: Yes skips the question, which
// In answers otherwise.
type RecoverOptions struct {
	Yes bool
	In  io.Reader
	Out io.Writer
}

func newRecover() *cobra.Command {
	var powerOff, yes bool
	recoverCommand := &cobra.Command{
		Use:   "recover --power-off",
		Short: "Go on with this host alone when the others are gone for good (an emergency)",
		Long: "For a cluster that lost the majority of its hosts for good: it powers every other host off\n" +
			"through Hetzner's API and labels its server clowder-retired, so it never takes part again, and\n" +
			"makes this host the cluster's only one, with the data of its volume copies. A volume with no\n" +
			"synced copy here waits for a restore. Then join new hosts as for a new cluster.\n" +
			"Never while the other hosts lead the cluster: use clowder host remove then.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !powerOff {
				return errors.New("recover powers every other host off for good: say so with --power-off")
			}
			files := Files{Dir: DefaultDir}
			parts := RecoverParts{
				Storage: hostStorage(files),
				KnowsLeader: func(ctx context.Context, address string, certificate tls.Certificate, ca *x509.Certificate) (bool, error) {
					return api.KnowsLeader(ctx, address, certificate, ca, buildinfo.Version)
				},
				Service: func(ctx context.Context, action string) error {
					if _, err := command.Run(ctx, time.Minute, "systemctl", action, "clowderd.service"); err != nil {
						return fmt.Errorf("%s clowderd.service: %w", action, err)
					}
					return nil
				},
			}
			options := RecoverOptions{Yes: yes, In: cmd.InOrStdin(), Out: cmd.OutOrStdout()}
			return Recover(cmd.Context(), files, parts, options, slog.Default())
		},
	}
	recoverCommand.Flags().BoolVar(&powerOff, "power-off", false, "power every other host off and retire its server")
	recoverCommand.Flags().BoolVar(&yes, "yes", false, "do it without asking")
	return recoverCommand
}

// recovery is one run of Recover: this host and what it works with.
type recovery struct {
	files       Files
	host        Host
	credentials credentials
	parts       RecoverParts
	options     RecoverOptions
	logger      *slog.Logger
}

// Recover makes this host the only one of its cluster: clowderd recover.
// In this order, each step safe to run again after a failure:
//  1. It stops the agent and reads the state from the Raft log on disk.
//  2. It refuses while another host knows a leader of the cluster, or
//     answers without saying; a host it does not reach is named in step 3.
//  3. It says what it does and asks (RecoverOptions).
//  4. For each other host: the label clowder-retired, then the power off,
//     both confirmed by Hetzner. A server the leader never checked is
//     checked first: the token may power off any server of the project.
//  5. Raft with this host as its only voter (raftnode.Recover), then the
//     command recover-remap, which this host commits as the new leader.
//  6. DRBD without the other hosts (storage.Driver.DropPeers).
//  7. It starts the agent, whose loops do the rest: the mesh, the Floating
//     IP, the placement, the forgetting of the removed hosts.
//
// Should it end before step 4, nothing changed, and it starts the agent
// again; from step 4 on, the agent stays stopped until a recover ends.
func Recover(ctx context.Context, files Files, parts RecoverParts, options RecoverOptions, logger *slog.Logger) (err error) {
	r := recovery{files: files, parts: parts, options: options, logger: logger}
	if r.host, err = files.readHost(); err != nil {
		return err
	}
	if r.credentials, err = files.readCredentials(); err != nil {
		return err
	}
	if err := r.parts.Service(ctx, "stop"); err != nil {
		return err
	}
	// changing is set right before the first step that changes something
	// (the retire, or the remap of a run after one): from there on the agent
	// stays stopped, as it would start with the old cluster's state
	changing := false
	defer func() {
		if err == nil {
			return
		}
		if changing {
			err = fmt.Errorf("%w (clowderd.service is stopped: run recover again)", err)
			return
		}
		// nothing changed: the agent runs on, also when the command was
		// interrupted
		restart, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		err = errors.Join(err, r.parts.Service(restart, "start"))
	}()
	current, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		return err
	}
	gone, err := hostsToRetire(current, r.host)
	if err != nil {
		return err
	}
	remap, err := state.NewCommand(state.KindRecoverRemap, time.Now(), state.LeaderActor,
		state.RecoverRemap{Survivor: r.host.ID})
	if err != nil {
		return err
	}
	if len(gone) > 0 {
		if err := r.ask(ctx, current, gone, remap); err != nil {
			return err
		}
	}
	changing = true
	if len(gone) > 0 {
		if err := r.retire(ctx, current, gone); err != nil {
			return err
		}
	}
	remap.At = time.Now().UTC() // the recover's time, not the question's
	if err := r.remap(ctx, remap); err != nil {
		return err
	}
	var peers []int
	for id := range current.Hosts {
		if id != r.host.ID {
			peers = append(peers, int(id))
		}
	}
	slices.Sort(peers)
	if err := r.parts.Storage.DropPeers(ctx, peers); err != nil {
		return err
	}
	if err := r.parts.Service(ctx, "start"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.options.Out, "Host %s is the cluster's only host now. Join two more as for a new cluster "+
		"(clowder host join-token); clowder get volumes shows the volumes that wait for a restore.\n", r.host.Name)
	return err
}

// hostsToRetire are the hosts recover powers off and retires: every host
// but this one that is not removed. None once a recover ran; a cluster of
// one host that never recovered has nothing to recover.
func hostsToRetire(current *state.State, self Host) ([]*state.Host, error) {
	if current.Keys == nil {
		return nil, errors.New("the cluster is not bootstrapped; there is nothing to recover")
	}
	if here, ok := current.Hosts[self.ID]; !ok || here.Phase == state.Removed {
		return nil, fmt.Errorf("host %s is not part of the cluster its log knows", self.Name)
	}
	var gone []*state.Host
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		if other := current.Hosts[id]; id != self.ID && other.Phase != state.Removed {
			gone = append(gone, other)
		}
	}
	switch {
	case len(gone) == 0 && current.RecoveredAt.IsZero():
		return nil, errors.New("this host is the cluster's only one; there is nothing to recover")
	case len(gone) > 0 && current.Config.Revision == 0:
		return nil, errors.New("no cluster.yaml brought the Hetzner token: recover can power no host off")
	}
	return gone, nil
}

// ask refuses while another host knows a leader or answers without saying,
// then says what recover does and asks for this host's name: whoever types
// it knows which host goes on alone.
func (r recovery) ask(ctx context.Context, current *state.State, gone []*state.Host, remap state.Command) error {
	var notReached []string
	for _, other := range gone {
		address := net.JoinHostPort(other.Underlay.String(), strconv.Itoa(r.host.APIPort))
		knows, err := r.parts.KnowsLeader(ctx, address, r.credentials.certificate, r.credentials.ca)
		switch {
		case errors.Is(err, api.ErrNotReached):
			notReached = append(notReached, other.Name)
		case err != nil:
			return fmt.Errorf("host %s answers, but not whether it knows a leader: %w", other.Name, err)
		case knows:
			return fmt.Errorf("host %s knows a leader of the cluster: it has its majority, and recover is only for "+
				"a cluster that lost it; remove hosts with clowder host remove", other.Name)
		}
	}
	text, err := r.describe(current, gone, notReached, remap)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(r.options.Out, text); err != nil || r.options.Yes {
		return err
	}
	if _, err := fmt.Fprintf(r.options.Out, "This cannot be undone. Type this host's name to go on: "); err != nil {
		return err
	}
	line, err := bufio.NewReader(r.options.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) { // at EOF the line is what was typed
		return fmt.Errorf("read the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != r.host.Name {
		return errors.New("not confirmed; nothing changed")
	}
	return nil
}

// describe says what recover does, with the outcome of the remap worked out
// on a copy of the state.
func (r recovery) describe(current *state.State, gone []*state.Host, notReached []string, remap state.Command) (string, error) {
	after, err := current.Copy()
	if err != nil {
		return "", err
	}
	if _, err := after.Apply(current.Index+1, remap); err != nil {
		return "", fmt.Errorf("work out the recover: %w", err)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Recover cluster %s with host %s alone. These hosts are powered off for good and their "+
		"servers labeled clowder-retired:\n", current.ClusterName, r.host.Name)
	for _, other := range gone {
		fmt.Fprintf(&text, "  %s (server %d, %s)\n", other.Name, other.ServerID, other.Phase)
	}
	if len(notReached) > 0 {
		fmt.Fprintf(&text, "Not reached, so not asked whether they lead: %s. Make sure they are down.\n",
			strings.Join(notReached, ", "))
	}
	var lost, unsynced, fresh []string
	for _, id := range slices.Sorted(maps.Keys(after.Volumes)) {
		volume, before := after.Volumes[id], current.Volumes[id]
		switch {
		case before.Status == state.VolumeRestorePending:
			// from an earlier recover: nothing new to say
		case volume.Status == state.VolumeRestorePending && len(volume.Copies) > 0:
			unsynced = append(unsynced, id)
		case volume.Status == state.VolumeRestorePending:
			lost = append(lost, id)
		case volume.DataGeneration != before.DataGeneration:
			fresh = append(fresh, id)
		}
	}
	if len(lost) > 0 {
		fmt.Fprintf(&text, "No copy here, so they wait for a restore: %s\n", strings.Join(lost, ", "))
	}
	if len(unsynced) > 0 {
		fmt.Fprintf(&text, "An unsynced copy here, kept as it is, so they wait for a restore: %s\n",
			strings.Join(unsynced, ", "))
	}
	if len(fresh) > 0 {
		fmt.Fprintf(&text, "Replicas 0, their data was elsewhere, so they start empty: %s\n", strings.Join(fresh, ", "))
	}
	return text.String(), nil
}

// retire labels each host's server clowder-retired and powers it off.
func (r recovery) retire(ctx context.Context, current *state.State, gone []*state.Host) error {
	client, err := controller.OpenHetzner(current, r.credentials.dataKey.Open)
	if err != nil {
		return err
	}
	for _, other := range gone {
		if err := retireHost(ctx, client, other); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(r.options.Out, "Host %s (server %d) is off and retired.\n", other.Name, other.ServerID); err != nil {
			return err
		}
	}
	return nil
}

// retireHost labels the host's server with its certificate's serial and
// powers it off. The label comes first: a server that is off but not
// labeled could be switched on and join its old peers again.
func retireHost(ctx context.Context, client *hetzner.Client, other *state.Host) error {
	ctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	if !other.ServerChecked {
		if err := client.CheckServer(ctx, other.ServerID, other.Underlay, other.PublicAddress); err != nil {
			return fmt.Errorf("host %s: %w", other.Name, err)
		}
	}
	if err := client.SetRetired(ctx, other.ServerID, other.CertificateSerial); err != nil {
		return fmt.Errorf("retire host %s: %w", other.Name, err)
	}
	if err := client.PowerOff(ctx, other.ServerID); err != nil {
		return fmt.Errorf("power host %s off: %w", other.Name, err)
	}
	return nil
}

// remap makes this host Raft's only voter and commits recover-remap as its
// leader. A second run finds the remap done: the state takes the command
// as unchanged, and Propose ends without an error.
func (r recovery) remap(ctx context.Context, remap state.Command) error {
	config := raftnode.Config{
		Dir: r.files.raftDir(), ID: r.host.ID, Bind: "127.0.0.1:0", Advertise: r.host.raftAddress(),
		Certificate: r.credentials.certificate, CA: r.credentials.ca, Logger: r.logger,
	}
	if err := raftnode.Recover(config); err != nil {
		return err
	}
	node, err := raftnode.Open(config)
	if err != nil {
		return err
	}
	defer func() { _ = node.Close() }() // the log is on disk
	ctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	if err := node.CatchUp(ctx); err != nil {
		return err
	}
	if _, err := node.Propose(ctx, remap); err != nil {
		return fmt.Errorf("remap the cluster to this host: %w", err)
	}
	return nil
}
