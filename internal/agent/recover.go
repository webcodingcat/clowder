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
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/raftnode"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// recoverTimeout limits each step of recover that waits for Hetzner or
// Raft: a power action, the new leader, the remap.
const recoverTimeout = 3 * time.Minute

// RecoverParts are what recover changes on the host and what it asks of
// other hosts; the tests pass fakes.
type RecoverParts struct {
	Storage interface {
		DiskStates(ctx context.Context) (map[string]string, error)
		DropPeers(ctx context.Context, peers []int) error
	}
	// Service runs a systemctl action ("stop", "start") on clowderd.service
	Service func(ctx context.Context, action string) error
	// Hosts asks the other hosts' agents, as this host
	// (api.NewRecoverCaller).
	Hosts func(certificate tls.Certificate, ca *x509.Certificate) OtherHosts
}

// OtherHosts are the agents of the other hosts, at an address of their
// underlay with the API port (api.RecoverCaller).
type OtherHosts interface {
	KnowsLeader(ctx context.Context, address string) (bool, error)
	Copies(ctx context.Context, address string) (map[string]string, error)
	Rejoin(ctx context.Context, address string, retired []state.HostID) error
}

// RecoverOptions say how recover gets its confirmation. With Yes, recover
// skips the question. Otherwise it reads the answer from In.
type RecoverOptions struct {
	Yes bool
	In  io.Reader
	Out io.Writer
}

func newRecover() *cobra.Command {
	var powerOff, yes bool
	recoverCommand := &cobra.Command{
		Use:   "recover --power-off",
		Short: "Go on with this host as the only control-plane host when the others are gone for good (an emergency)",
		Long: "For a cluster that lost the majority of its voters for good: it powers every host off that takes\n" +
			"no connection, through Hetzner's API, and labels its server clowder-retired, so it never takes part\n" +
			"again. Workers that answer stay with their stacks and volume copies. This host becomes the only\n" +
			"control-plane host. A volume with no synced copy left waits for a restore. Then join new\n" +
			"control-plane hosts. Never while the other hosts lead the cluster: use clowder host remove then.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !powerOff {
				return errors.New("recover powers every host off that takes no connection, for good: say so with --power-off")
			}
			files := Files{Dir: DefaultDir}
			parts := RecoverParts{
				Storage: hostStorage(files),
				Hosts: func(certificate tls.Certificate, ca *x509.Certificate) OtherHosts {
					return api.NewRecoverCaller(certificate, ca, buildinfo.Version)
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
	recoverCommand.Flags().BoolVar(&powerOff, "power-off", false, "power every host off that takes no connection, and retire its server")
	recoverCommand.Flags().BoolVar(&yes, "yes", false, "do it without asking")
	return recoverCommand
}

type recovery struct {
	files       Files
	host        Host
	credentials credentials
	parts       RecoverParts
	options     RecoverOptions
	logger      *slog.Logger
	others      OtherHosts
}

// recoverPlan is what the answers of the other hosts decided. The hosts of
// a recover fall into these sets:
//   - others (otherHosts): every host in the log but this one that is not
//     removed; ask splits them into retired and stay;
//   - retired: took no connection; this run powers them off and removes
//     them;
//   - stay: workers that answered; they keep their stacks and copies;
//   - gone (goneHosts): every host but this one and those that stay, so
//     retired plus the hosts that an earlier run removed. DRBD may still
//     list any of them as a peer.
type recoverPlan struct {
	retired, stay []*state.Host
	// synced are the copies that DRBD found synced on this host and on the
	// hosts that stay
	synced []state.SyncedCopy
}

// Recover makes this host the only control-plane host of its cluster
// (clowderd recover; the steps are in docs/developing/cluster.md). Each
// step is safe to run again after a failure.
func Recover(ctx context.Context, files Files, parts RecoverParts, options RecoverOptions, logger *slog.Logger) (err error) {
	r := recovery{files: files, parts: parts, options: options, logger: logger}
	if r.host, err = files.readHost(); err != nil {
		return err
	}
	if err := r.host.checkControlPlane("recover"); err != nil {
		return err
	}
	if r.credentials, err = files.readCredentials(r.host.Roles); err != nil {
		return err
	}
	r.others = parts.Hosts(r.credentials.certificate, r.credentials.ca)
	if err := r.parts.Service(ctx, "stop"); err != nil {
		return err
	}
	// once changing is set, the agent stays stopped, because it would start
	// with the old cluster's state
	changing := false
	defer func() {
		if err == nil {
			return
		}
		if changing {
			err = fmt.Errorf("%w (clowderd.service is stopped: run recover again)", err)
			return
		}
		// nothing changed: start the agent again, even if the command was
		// interrupted
		restart, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		err = errors.Join(err, r.parts.Service(restart, "start"))
	}()
	current, err := raftnode.ReadState(files.raftDir())
	if err != nil {
		return err
	}
	others, err := otherHosts(current, r.host)
	if err != nil {
		return err
	}
	plan, err := r.ask(ctx, current, others)
	if err != nil {
		return err
	}
	// this host's copies count by DRBD as the workers' do: its state may be
	// behind on them
	disks, err := r.parts.Storage.DiskStates(ctx)
	if err != nil {
		return fmt.Errorf("read the copies on this host: %w", err)
	}
	plan.synced = append(syncedCopies(current, r.host.ID, disks), plan.synced...)
	remap, err := state.NewCommand(state.KindRecoverRemap, time.Now(), state.LeaderActor, plan.payload(r.host.ID))
	if err != nil {
		return err
	}
	// a run again after a failure may have nothing left to retire, and
	// then nothing new to confirm
	if len(plan.retired) > 0 {
		if err := r.confirm(current, plan, remap); err != nil {
			return err
		}
	}
	changing = true
	if err := r.retire(ctx, current, plan.retired); err != nil {
		return err
	}
	remap.At = time.Now().UTC() // the recover's time, not the question's
	if err := r.remap(ctx, remap); err != nil {
		return err
	}
	// gone, not only retired: an earlier run may have failed before DRBD
	// dropped the hosts it retired
	gone := goneHosts(current, r.host.ID, plan.stay)
	if err := r.parts.Storage.DropPeers(ctx, drbdPeers(gone)); err != nil {
		return err
	}
	// before this host leads: a worker with a higher term would end its lead
	if err := r.rejoin(ctx, plan.stay, gone); err != nil {
		return err
	}
	if err := r.parts.Service(ctx, "start"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.options.Out, "Host %s is the cluster's only %s host now. Join two more as for a new "+
		"cluster (clowder host join-token --roles %s); clowder get volumes shows the volumes that wait for a restore.\n",
		r.host.Name, hostrole.ControlPlane, hostrole.ControlPlane)
	return err
}

// otherHosts returns the hosts that are not removed, except this one. It
// returns none, without an error, after a recover has run and kept no
// host.
func otherHosts(current *state.State, self Host) ([]*state.Host, error) {
	if current.Keys == nil {
		return nil, errors.New("the cluster is not bootstrapped; there is nothing to recover")
	}
	if here, ok := current.Hosts[self.ID]; !ok || here.Phase == state.Removed {
		return nil, fmt.Errorf("host %s is not part of the cluster its log knows", self.Name)
	}
	var others []*state.Host
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		if other := current.Hosts[id]; id != self.ID && other.Phase != state.Removed {
			others = append(others, other)
		}
	}
	switch {
	case len(others) == 0 && current.RecoveredAt.IsZero():
		return nil, errors.New("this host is the cluster's only one; there is nothing to recover")
	case len(others) > 0 && !current.Configured():
		return nil, errors.New("no ClusterConfiguration names the Hetzner token: recover can power no host off")
	}
	return others, nil
}

// ask decides each other host's part from its answer. A host that takes no
// connection is retired. Recover refuses while another host knows a
// leader, or answers without saying whether it does, and while a
// control-plane host answers: the state's Voter may be behind Raft's
// configuration, so any control-plane host may hold a vote of the old
// majority. A worker that answers stays, and it tells its copies.
func (r recovery) ask(ctx context.Context, current *state.State, others []*state.Host) (recoverPlan, error) {
	var plan recoverPlan
	for _, other := range others {
		address := r.address(other)
		knows, err := r.others.KnowsLeader(ctx, address)
		switch {
		case errors.Is(err, api.ErrNotReached):
			plan.retired = append(plan.retired, other)
			continue
		case err != nil:
			return plan, fmt.Errorf("host %s answers, but not whether it knows a leader: %w", other.Name, err)
		case knows:
			return plan, fmt.Errorf("host %s knows a leader of the cluster: it has its majority, and recover is only for "+
				"a cluster that lost it; remove hosts with clowder host remove (if this host led until now, try "+
				"again in a minute, once the others notice that its agent stopped)", other.Name)
		case other.Roles.Has(hostrole.ControlPlane):
			return plan, fmt.Errorf("host %s is a %s host and answers, so it may hold a vote of the old majority: "+
				"power its server off, or bring the majority back", other.Name, hostrole.ControlPlane)
		}
		disks, err := r.others.Copies(ctx, address)
		if err != nil {
			return plan, fmt.Errorf("host %s stays, but does not tell its copies: %w", other.Name, err)
		}
		plan.stay = append(plan.stay, other)
		plan.synced = append(plan.synced, syncedCopies(current, other.ID, disks)...)
	}
	if len(plan.retired) == 0 && current.RecoveredAt.IsZero() {
		return plan, errors.New("every other host answers: no host is gone, and there is nothing to recover")
	}
	return plan, nil
}

func (r recovery) address(other *state.Host) string {
	// every host has the same API port
	return net.JoinHostPort(other.Underlay.String(), strconv.Itoa(r.host.APIPort))
}

// syncedCopies are the copies with a disk on the host whose DRBD resource
// is UpToDate there. The resource's name holds the data generation, so a
// copy of another generation than the state's does not count. A plain LV
// (replicas 0) needs no report: the remap keeps it where it is.
func syncedCopies(current *state.State, host state.HostID, disks map[string]string) []state.SyncedCopy {
	var synced []state.SyncedCopy
	for _, id := range slices.Sorted(maps.Keys(current.Volumes)) {
		volume := current.Volumes[id]
		hasCopy := slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host && !c.Tiebreaker })
		if volume.Minor != nil && hasCopy && disks[names.VolumeName(id, volume.DataGeneration)] == storage.DiskUpToDate {
			synced = append(synced, state.SyncedCopy{Volume: id, DataGeneration: volume.DataGeneration, Host: host})
		}
	}
	return synced
}

// payload is the plan as recover-remap's payload.
func (p recoverPlan) payload(survivor state.HostID) state.RecoverRemap {
	remap := state.RecoverRemap{Survivor: survivor, Synced: p.synced}
	for _, host := range p.retired {
		remap.Retired = append(remap.Retired, host.ID)
	}
	return remap
}

// goneHosts returns the IDs of the set gone (recoverPlan).
func goneHosts(current *state.State, self state.HostID, stay []*state.Host) []state.HostID {
	var gone []state.HostID
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		if id != self && !slices.ContainsFunc(stay, func(host *state.Host) bool { return host.ID == id }) {
			gone = append(gone, id)
		}
	}
	return gone
}

// drbdPeers turns host IDs into DRBD's node-ids, which are the same
// numbers.
func drbdPeers(hosts []state.HostID) []int {
	peers := make([]int, 0, len(hosts))
	for _, id := range hosts {
		peers = append(peers, int(id))
	}
	return peers
}

// confirm asks for this host's name: whoever types it knows which host
// goes on.
func (r recovery) confirm(current *state.State, plan recoverPlan, remap state.Command) error {
	text, err := r.describe(current, plan, remap)
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

func (r recovery) describe(current *state.State, plan recoverPlan, remap state.Command) (string, error) {
	after := current.Clone()
	if _, err := after.Apply(current.Index+1, remap); err != nil {
		return "", fmt.Errorf("work out the recover: %w", err)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Recover cluster %s with host %s as its only %s host. These hosts take no connection; "+
		"they are powered off for good and their servers labeled clowder-retired. Make sure they are down:\n",
		current.ClusterName, r.host.Name, hostrole.ControlPlane)
	for _, other := range plan.retired {
		fmt.Fprintf(&text, "  %s (server %d, %s)\n", other.Name, other.ServerID, other.Phase)
	}
	if len(plan.stay) > 0 {
		text.WriteString("These workers answer and stay, with their stacks and synced copies:\n")
		for _, other := range plan.stay {
			fmt.Fprintf(&text, "  %s (%s)\n", other.Name, other.Phase)
		}
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
		fmt.Fprintf(&text, "No copy left, so they wait for a restore: %s\n", strings.Join(lost, ", "))
	}
	if len(unsynced) > 0 {
		fmt.Fprintf(&text, "Only unsynced copies left, kept as they are, so they wait for a restore: %s\n",
			strings.Join(unsynced, ", "))
	}
	if len(fresh) > 0 {
		fmt.Fprintf(&text, "Replicas 0, their data was on a retired host, so they start empty: %s\n",
			strings.Join(fresh, ", "))
	}
	return text.String(), nil
}

func (r recovery) retire(ctx context.Context, current *state.State, retired []*state.Host) error {
	if len(retired) == 0 {
		return nil
	}
	client, err := controller.OpenHetzner(current, r.credentials.decrypt)
	if err != nil {
		return err
	}
	for _, other := range retired {
		if err := retireHost(ctx, client, other); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(r.options.Out, "Host %s (server %d) is off and retired.\n", other.Name, other.ServerID); err != nil {
			return err
		}
	}
	return nil
}

// retireHost labels the server before it powers it off: a server that is
// off but not labeled could be switched on and join its old peers again.
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

// remap may run twice: the second time, the state treats the command as
// changing nothing, and Propose returns without an error.
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

// rejoin tells each worker that stays to drop the gone hosts and to follow
// this host with a fresh Raft log (rejoiner).
func (r recovery) rejoin(ctx context.Context, stay []*state.Host, gone []state.HostID) error {
	for _, other := range stay {
		if err := r.others.Rejoin(ctx, r.address(other), gone); err != nil {
			return fmt.Errorf("tell host %s to follow this host: %w", other.Name, err)
		}
		if _, err := fmt.Fprintf(r.options.Out, "Host %s stays and follows this host.\n", other.Name); err != nil {
			return err
		}
	}
	return nil
}
