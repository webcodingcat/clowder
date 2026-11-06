package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/hetzner"
	"github.com/webcodingcat/clowder/internal/state"
)

// AskSilence asks a voter how long it has not heard from a host
// (Heard.Silence, through the agent's API).
type AskSilence func(ctx context.Context, voter, host state.HostID) (time.Duration, error)

const (
	// askTimeout limits the question to one voter. A voter that does not
	// answer in time does not count (see voters).
	askTimeout = 2 * time.Second
	// powerOffTries is how many times the leader tries to power off a
	// server before it gives up the fence. Within each try, hetzner.Client
	// itself retries while the API answers that it is busy.
	powerOffTries = 3
	// powerOffPause is the pause after the first failed try; it doubles
	// after each further try.
	powerOffPause = 5 * time.Second
	// powerOffTimeout limits one try, including the client's own retries.
	// A power action takes seconds, and the loop watches no beats while a
	// fence is trying.
	powerOffTimeout = 45 * time.Second
	// fenceAgain is how long the leader waits before it tries again to
	// fence a host that it could not power off. A user's fence-confirm does
	// not have to wait.
	fenceAgain = 5 * time.Minute
	// powerOnAgain is how long the leader waits before it asks Hetzner
	// again to power on a server that it failed to power on.
	powerOnAgain = 30 * time.Second
)

// fencer keeps its records in memory only, so a new leader may power on a
// fenced host a second time, which does no harm.
type fencer struct {
	FenceParts

	// fenceAfter and powerOnAfter hold, by host ID, when the leader may try
	// again to power a host off or on after a failed try. They are
	// separate, so a failed power-off does not hold up a power-on after a
	// user's fence-confirm.
	fenceAfter, powerOnAfter map[state.HostID]time.Time
	// pause is powerOffPause; tests shorten it
	pause time.Duration
	// poweredOn holds the fenced hosts that this leader has powered on
	poweredOn map[state.HostID]bool
}

// FenceParts holds what the fencing loop gets from the agent.
type FenceParts struct {
	Cluster    Cluster
	Propose    Propose
	Open       Open
	Heard      *Heard
	Ask        AskSilence
	Self       state.HostID
	Conditions *Conditions
	Logger     *slog.Logger
}

// RunFencing fails, fences and powers on hosts by their beats while this
// agent leads, until ctx ends.
func RunFencing(ctx context.Context, parts FenceParts) {
	f := &fencer{
		FenceParts: parts, pause: powerOffPause,
		fenceAfter: map[state.HostID]time.Time{}, powerOnAfter: map[state.HostID]time.Time{},
		poweredOn: map[state.HostID]bool{},
	}
	for {
		changed := f.Cluster.Changed()
		if f.Cluster.IsLeader() {
			if err := f.pass(ctx); err != nil {
				f.Logger.Error("watch the hosts", "error", err)
			}
		} else {
			clear(f.fenceAfter)
			clear(f.powerOnAfter)
			clear(f.poweredOn)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(BeatInterval):
		}
	}
}

// pass works on one copy of the state, so a change that depends on another
// change's commit happens in the next pass.
func (f *fencer) pass(ctx context.Context) error {
	copied := f.Cluster.SharedCopy()
	now := time.Now()
	return errors.Join(f.watch(ctx, copied), f.fence(ctx, copied, now), f.powerOn(ctx, copied, now))
}

// watch needs a majority of the voters for each change, so a host that only
// the leader cannot hear (a cut between the two) does not fail.
func (f *fencer) watch(ctx context.Context, copied *state.State) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		host := copied.Hosts[id]
		var to state.HostPhase
		switch {
		case id == f.Self:
			continue
		case state.Up(host.Phase):
			// ask the voters only once the leader itself misses the host's beats
			if f.Heard.Silence(id) < FailAfter || !f.voters(ctx, copied, id, func(silence time.Duration) bool {
				return silence >= FailAfter
			}) {
				continue
			}
			to = state.Failed
		case host.Phase == state.Failed:
			if f.Heard.Silence(id) >= backWithin || !f.voters(ctx, copied, id, func(silence time.Duration) bool {
				return silence < backWithin
			}) {
				continue
			}
			to = state.Active
		default:
			continue
		}
		f.Logger.Warn("a host's beats changed its phase", "host", host.Name, "from", host.Phase, "to", to)
		if err := movePhase(ctx, f.Propose, host, to); err != nil {
			errs = append(errs, fmt.Errorf("host %s %s: %w", host.Name, to, err))
		}
	}
	return errors.Join(errs...)
}

// voters reports whether a majority of the voters agree on the host's
// silence. A voter that does not answer does not count, and neither does
// the host itself, so an unreachable voter keeps the host as it is.
func (f *fencer) voters(ctx context.Context, copied *state.State, host state.HostID, agrees func(time.Duration) bool) bool {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	var (
		mu    sync.Mutex
		count int
		asked sync.WaitGroup
	)
	total := 0
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		voter := copied.Hosts[id]
		if !voter.Voter || voter.Phase == state.Removed {
			continue
		}
		total++
		switch id {
		case host:
			continue // the host being judged has no say about itself
		case f.Self:
			if agrees(f.Heard.Silence(host)) {
				count++
			}
			continue
		}
		asked.Go(func() {
			silence, err := f.Ask(ctx, id, host)
			if err != nil {
				f.Logger.Debug("a voter did not say whether it hears a host", "voter", id, "host", host, "error", err)
				return
			}
			if agrees(silence) {
				mu.Lock()
				count++
				mu.Unlock()
			}
		})
	}
	asked.Wait()
	return majority(count, total)
}

func (f *fencer) fence(ctx context.Context, copied *state.State, now time.Time) error {
	// one fence at a time: while one runs, the other failed hosts wait, and
	// their conditions stay as they were
	for _, id := range slices.Sorted(maps.Keys(copied.Operations)) {
		if operation := copied.Operations[id]; operation.Fence != nil && operation.State == state.OpRunning {
			return f.step(ctx, copied, operation, now)
		}
	}
	var errs []error
	started := false
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		host := copied.Hosts[id]
		if host.Phase != state.Failed || !needsFence(copied, id) {
			f.Conditions.Observe(fencingBlockedKey(id), false)
			continue
		}
		if started {
			continue // one at a time: this host comes later
		}
		reason := f.whyNotFence(copied, host, now)
		if f.Conditions.Observe(fencingBlockedKey(id), reason != "") {
			f.Logger.Error("a failed host cannot be fenced; its stacks stay where they are", "host", host.Name,
				"reason", reason)
		}
		if reason != "" {
			continue
		}
		started = true
		f.Logger.Warn("fencing a failed host", "host", host.Name, "server", host.ServerID)
		_, err := commit(ctx, f.Propose, state.KindStartOperation, state.StartOperation{
			Kind: state.OperationFenceHost, Locks: []string{state.HostLock(id)}, Fence: &state.HostFence{Host: id},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("fence host %s: %w", host.Name, err))
		}
	}
	return errors.Join(errs...)
}

// needsFence reports whether a failed host must be powered off: only then
// may another host start its stacks. A power cycle gains nothing for a host
// without stacks or without another host to take them.
func needsFence(copied *state.State, id state.HostID) bool {
	hasStack, other := false, false
	for _, stack := range copied.Stacks {
		hasStack = hasStack || (stack.Host != nil && *stack.Host == id)
	}
	for _, host := range copied.Hosts {
		other = other || (host.ID != id && host.Phase == state.Active && !host.Leaving)
	}
	return hasStack && other
}

// whyNotFence returns why a failed host may not be fenced now, or "" if it
// may. An unchecked server ID might power off another server. When several
// hosts fail at once, the network is the likely cause, and powering hosts
// off would not help.
func (f *fencer) whyNotFence(copied *state.State, host *state.Host, now time.Time) string {
	total, active := 0, 0
	for _, voter := range copied.Hosts {
		if !voter.Voter || voter.Phase == state.Removed {
			continue
		}
		total++
		// a recovering voter does not count: it was down a moment ago
		if voter.ID != host.ID && state.InService(voter.Phase) {
			active++
		}
	}
	switch {
	case !copied.Configured():
		return "no ClusterConfiguration names the Hetzner token to power it off"
	case !host.ServerChecked:
		return "Hetzner has not confirmed its server"
	case copied.HostLocked(host.ID):
		return "an operation holds it"
	case !majority(active, total):
		return fmt.Sprintf("only %d of %d voters would be left active; more hosts failed at once", active, total)
	case now.Before(f.fenceAfter[host.ID]):
		return "powering it off failed a short while ago"
	}
	return ""
}

func (f *fencer) step(ctx context.Context, copied *state.State, operation *state.Operation, now time.Time) error {
	// the host exists: its lock keeps it from being removed
	host := copied.Hosts[operation.Fence.Host]
	switch {
	case operation.Step == state.FenceStart && host.Phase == state.Fencing:
		return f.powerOff(ctx, copied, operation, host)
	case operation.Step == state.FenceStart && host.Phase == state.Fenced:
		// fenced by a leader that stopped before it recorded the step
		return f.advance(ctx, operation)
	case operation.Step == state.FenceStart:
		// moved back to failed by a leader that stopped before it finished
		return f.finish(ctx, operation, true, "the server could not be powered off")
	// FenceOff: the step got its own deadline (StepTime) when it was
	// recorded
	case now.After(operation.Deadline) || stacksMovedOff(copied, operation.Fence):
		f.Logger.Info("the fence of a host is over", "host", host.Name)
		return f.finish(ctx, operation, false, "")
	}
	return nil
}

// powerOff records the phase fenced before the step; step handles a leader
// that stopped in between.
func (f *fencer) powerOff(ctx context.Context, copied *state.State, operation *state.Operation, host *state.Host) error {
	client, err := OpenHetzner(copied, f.Open)
	if err == nil {
		err = f.tryPowerOff(ctx, client, host)
		f.Conditions.Observe(hetznerTokenRefused, hetzner.TokenRefused(err))
	}
	if err != nil {
		// count from now, not from the pass's start: the tries took a while
		f.fenceAfter[host.ID] = time.Now().Add(fenceAgain)
		f.Logger.Error("a failed host's server could not be powered off; it stays failed", "host", host.Name,
			"error", err)
		back := state.SetHostPhase{Host: host.ID, Phase: state.Failed, From: state.Fencing, Operation: operation.ID}
		if _, commitErr := commit(ctx, f.Propose, state.KindSetHostPhase, back); commitErr != nil {
			return fmt.Errorf("take host %s back to failed: %w", host.Name, commitErr)
		}
		return f.finish(ctx, operation, true, err.Error())
	}
	f.Logger.Warn("a failed host's server is off; its stacks start elsewhere", "host", host.Name)
	fenced := state.SetHostPhase{Host: host.ID, Phase: state.Fenced, From: state.Fencing, Operation: operation.ID}
	if _, err := commit(ctx, f.Propose, state.KindSetHostPhase, fenced); err != nil {
		return fmt.Errorf("record host %s fenced: %w", host.Name, err)
	}
	return f.advance(ctx, operation)
}

// tryPowerOff holds up the loop for at most powerOffTries times
// powerOffTimeout plus the pauses (about two and a half minutes). No beats
// are watched meanwhile, but only one fence runs anyway. The lead is
// confirmed before each try, because a leader that is cut off from the
// others must not power off a host that the others still run.
func (f *fencer) tryPowerOff(ctx context.Context, client *hetzner.Client, host *state.Host) error {
	pause := f.pause
	var err error
	for try := 1; try <= powerOffTries; try++ {
		if err = f.Cluster.VerifyLeader(ctx); err != nil {
			return err
		}
		tryCtx, cancel := context.WithTimeout(ctx, powerOffTimeout)
		err = client.PowerOff(tryCtx, host.ServerID)
		cancel()
		if err == nil || try == powerOffTries {
			break
		}
		f.Logger.Warn("powering a failed host's server off failed; trying again", "host", host.Name, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		pause *= 2
	}
	return err
}

// stacksMovedOff also counts a stack that waits, for example for this host
// because its data has no other copy.
func stacksMovedOff(copied *state.State, fence *state.HostFence) bool {
	for _, id := range fence.Stacks {
		stack, ok := copied.Stacks[id]
		switch {
		case !ok:
		case stack.Host == nil || *stack.Host == fence.Host:
			if stack.WaitReason == "" {
				return false // not placed yet
			}
		case stack.Status != state.StackRunning:
			return false
		}
	}
	return true
}

func (f *fencer) advance(ctx context.Context, operation *state.Operation) error {
	_, err := commit(ctx, f.Propose, state.KindAdvanceOperation, state.AdvanceOperation{
		ID: operation.ID, FromStep: state.FenceStart, Step: state.FenceOff,
	})
	if err != nil {
		return fmt.Errorf("operation %d: record the server off: %w", operation.ID, err)
	}
	return nil
}

func (f *fencer) finish(ctx context.Context, operation *state.Operation, failed bool, why string) error {
	_, err := commit(ctx, f.Propose, state.KindFinishOperation, state.FinishOperation{
		ID: operation.ID, Failed: failed, Error: why,
	})
	if err != nil {
		return fmt.Errorf("finish operation %d: %w", operation.ID, err)
	}
	return nil
}

// powerOn powers on each fenced host once per leader: a host that does not
// come back after Hetzner powered it on is likely broken. It stays fenced,
// and its copies are made anew elsewhere after scheduler.CopyWait.
func (f *fencer) powerOn(ctx context.Context, copied *state.State, now time.Time) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(copied.Hosts)) {
		host := copied.Hosts[id]
		switch {
		case host.Phase != state.Fenced:
			delete(f.poweredOn, id)
			continue
		case copied.HostLocked(id) || f.poweredOn[id] || now.Before(f.powerOnAfter[id]):
			continue
		case !copied.Configured() || !host.ServerChecked:
			// a user confirmed the fence. A server ID that Hetzner did not
			// confirm may name another server, so the user powers the
			// server on (host-fenced).
			continue
		case host.Flapping(now):
			f.Logger.Error("a host was fenced too often; it stays off until a user enables it", "host", host.Name,
				"times", state.FlapLimit, "within", state.FlapWindow)
			if err := movePhase(ctx, f.Propose, host, state.Disabled); err != nil {
				errs = append(errs, fmt.Errorf("disable host %s: %w", host.Name, err))
			}
			continue
		}
		client, err := OpenHetzner(copied, f.Open)
		if err == nil {
			powerCtx, cancel := context.WithTimeout(ctx, hetznerTimeout)
			err = client.PowerOn(powerCtx, host.ServerID)
			cancel()
			f.Conditions.Observe(hetznerTokenRefused, hetzner.TokenRefused(err))
		}
		if err != nil {
			f.powerOnAfter[id] = now.Add(powerOnAgain)
			errs = append(errs, fmt.Errorf("power host %s on: %w", host.Name, err))
			continue
		}
		f.poweredOn[id] = true
		f.Logger.Info("a fenced host is powered on again", "host", host.Name)
	}
	return errors.Join(errs...)
}
