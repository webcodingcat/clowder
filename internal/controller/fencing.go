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
// (Heard.Silence, over the agent's API).
type AskSilence func(ctx context.Context, voter, host state.HostID) (time.Duration, error)

const (
	// askTimeout bounds the question to one voter. A voter that does not
	// answer in time counts as one that heard from the host.
	askTimeout = 2 * time.Second
	// powerOffTries is how often the leader tries to power off a server
	// before it gives up the fence. Within each try, hetzner.Client itself
	// retries when the API answers that it is busy.
	powerOffTries = 3
	// powerOffPause is the pause after the first failed try; it doubles
	// after each further try.
	powerOffPause = 5 * time.Second
	// powerOffTimeout bounds one try, including the client's own retries.
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

// fencer detects failed hosts, fences them and powers them on again while
// this agent leads. It keeps its records in memory, so a new leader starts
// with an empty memory. The new leader may then power on a fenced host a
// second time, which changes nothing.
type fencer struct {
	FenceParts

	// fenceAfter and powerOnAfter say, by host ID, when the leader may try
	// again to power a host off or on after a failed try. They are kept
	// apart, so a failed power-off does not hold up a power-on after a
	// user's fence-confirm.
	fenceAfter, powerOnAfter map[state.HostID]time.Time
	// pause is powerOffPause; tests shorten it
	pause time.Duration
	// poweredOn are the fenced hosts that this leader powered on
	poweredOn map[state.HostID]bool
}

// FenceParts are what the fencing loop works with, from the agent.
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

// RunFencing keeps the hosts' phases in line with their beats while this
// agent leads, until ctx ends. It looks every BeatInterval
// (docs/developing/loops.md, Fencing):
//   - A host becomes failed when a majority of the voters has not heard
//     from it for FailAfter. A failed host becomes active again when a
//     majority hears from it again.
//   - Failed hosts are fenced one at a time, and only while a majority of
//     the voters is active without the host. A fence-host operation powers
//     off the host's server. Only then is the host fenced, and its stacks
//     start elsewhere (placement).
//   - A fenced host is powered on again once its fence is over. It comes
//     back as recovering, and becomes active once its copies have caught
//     up. A host fenced FlapLimit times within FlapWindow stays off
//     (disabled).
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

// pass makes each of the changes once, on the same copy of the state. A
// change that depends on the commit of another one happens in the next pass.
func (f *fencer) pass(ctx context.Context) error {
	copied := f.Cluster.SharedCopy()
	now := time.Now()
	return errors.Join(f.watch(ctx, copied), f.fence(ctx, copied, now), f.powerOn(ctx, copied, now))
}

// watch moves the hosts whose beats stopped to failed, and moves the failed
// hosts that are heard again back to active. Both need a majority of the
// voters, the leader included. So a host that only the leader cannot hear,
// for example because the network between the two is cut, is not failed.
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

// voters tells whether says is true of a host's silence for a majority of
// the voters. The leader counts itself and asks the others at the same
// time. A voter that does not answer does not count, and neither does the
// host itself. So a voter that cannot be asked keeps a host from becoming
// failed, and also from becoming active again: either way the host stays
// as it is.
func (f *fencer) voters(ctx context.Context, copied *state.State, host state.HostID, says func(time.Duration) bool) bool {
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
			continue // a host does not hear itself
		case f.Self:
			if says(f.Heard.Silence(host)) {
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
			if says(silence) {
				mu.Lock()
				count++
				mu.Unlock()
			}
		})
	}
	asked.Wait()
	return majority(count, total)
}

// fence moves the fencing forward. If a fence-host operation is running, it
// takes that one on. Otherwise it starts one for the first failed host that
// may be fenced.
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
			continue // one at a time: it comes next
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

// needsFence tells whether a failed host must be powered off. That is the
// case when it has a stack that another active host could take over: that
// host must not start the stack while the failed one may still run it. A
// power cycle gains nothing for a host without stacks (such as one that was
// drained and is rebooting for maintenance) or for a host whose stacks no
// other host could take. Such a host stays failed, with its alert, and
// becomes active again once a majority hears from it.
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

// whyNotFence says why a failed host may not be fenced now, or "" if it
// may. A host may be fenced when all of these hold:
//   - Hetzner can power it off: the configuration names the token, and the
//     leader has checked the host's server (a wrong ID would power off
//     another server).
//   - A majority of the voters stays active without it. When several hosts
//     fail at once, the network is the more likely cause, and powering off
//     hosts would not help.
//   - The leader did not fail to power it off a short while ago.
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

// step moves a fence-host operation forward. At FenceStart it powers off
// the server and, once the server is off, fences the host. If it cannot,
// the host goes back to failed and the operation fails. At FenceOff it
// finishes once the host's stacks run elsewhere, or after the step's
// deadline.
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
	// FenceOff: the step got its own StepTime when it was recorded
	case now.After(operation.Deadline) || stacksMovedOff(copied, operation.Fence):
		f.Logger.Info("the fence of a host is over", "host", host.Name)
		return f.finish(ctx, operation, false, "")
	}
	return nil
}

// powerOff powers off the host's server, with up to powerOffTries tries.
// Then it fences the host (first the phase fenced, then the step). If it
// cannot, it moves the host back to failed and ends the operation. The
// leader confirms its lead before each try: a leader that is cut off from
// the others must not power off a host that the others still run.
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

// tryPowerOff powers off the server, with up to powerOffTries tries. Each
// try ends after powerOffTimeout, including the client's own retries of a
// busy API. So a fence that cannot power off holds up the loop for at most
// powerOffTries times powerOffTimeout plus the pauses (about two and a half
// minutes). No beats are watched meanwhile, but only one fence runs anyway.
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

// stacksMovedOff tells whether the stacks that a fenced host had now run
// elsewhere. Each stack must be gone, run on another host, or wait for this
// host because its data has no other copy.
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

// advance records FenceOff: the server is off.
func (f *fencer) advance(ctx context.Context, operation *state.Operation) error {
	_, err := commit(ctx, f.Propose, state.KindAdvanceOperation, state.AdvanceOperation{
		ID: operation.ID, FromStep: state.FenceStart, Step: state.FenceOff,
	})
	if err != nil {
		return fmt.Errorf("operation %d: record the server off: %w", operation.ID, err)
	}
	return nil
}

// finish ends a fence-host operation.
func (f *fencer) finish(ctx context.Context, operation *state.Operation, failed bool, why string) error {
	_, err := commit(ctx, f.Propose, state.KindFinishOperation, state.FinishOperation{
		ID: operation.ID, Failed: failed, Error: why,
	})
	if err != nil {
		return fmt.Errorf("finish operation %d: %w", operation.ID, err)
	}
	return nil
}

// powerOn powers on each fenced host whose fence is over, once per leader.
// A server that does not come up is likely broken, so it is not asked again
// until another agent leads. It stays fenced, and its copies are made anew
// elsewhere after scheduler.CopyWait. A host that the flapping brake keeps
// off is disabled instead, until a user enables it.
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
			// confirm may name another server, so the user powers it on
			// (host-fenced).
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
