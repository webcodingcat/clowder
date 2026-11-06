package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/notify"
)

// noLeaderFor is how long an agent must know no leader before it sends an
// alert by itself.
const noLeaderFor = 2 * time.Minute

// QuorumWatch sends alerts from this agent while it knows no Raft leader.
// Without a majority of the voters there is no leader, so the leader's
// alerts are silent. That happens, for example, when two hosts of three
// are down, or when this host is cut off. The watch alerts the channels
// once there has been no leader for noLeaderFor, reminds them every
// remindEvery, and sends the all-clear when a leader is back. It takes the
// channels and their secrets from this agent's own state, the last one it
// applied. It keeps what it sent in memory only, so an agent that restarts
// without a leader alerts the channels again.
type QuorumWatch struct {
	alerts *Alerts
	host   string
	// leader reports whether this agent knows a leader
	// (raftnode.Node.Leader)
	leader func() bool

	// Only the watch's loop uses the fields below.

	// lostAt is when this agent last stopped knowing a leader; zero while
	// it knows one
	lostAt time.Time
	// open is true once the alert is raised; toldAt is when the watch last
	// queued a message
	open   bool
	toldAt time.Time
	// pending holds, by channel index, the message that the channel has
	// not taken yet; a newer message replaces it
	pending map[int]notify.Message
}

// NewQuorumWatch makes the watch for the agent on the host with the given
// name.
func NewQuorumWatch(alerts *Alerts, host string, leader func() bool) *QuorumWatch {
	return &QuorumWatch{alerts: alerts, host: host, leader: leader, pending: map[int]notify.Message{}}
}

// Run runs a round every alertsInterval until ctx ends.
func (w *QuorumWatch) Run(ctx context.Context) {
	for {
		w.round(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(alertsInterval):
		}
	}
}

// round checks whether this agent knows a leader, queues the message that
// is due for each channel, and sends the pending messages. A channel that
// fails gets its message again in the next round.
func (w *QuorumWatch) round(ctx context.Context, now time.Time) {
	if w.leader() {
		if w.open {
			w.tell(notify.Resolved, now)
			w.open = false
		}
		w.lostAt = time.Time{}
	} else {
		if w.lostAt.IsZero() {
			w.lostAt = now
		}
		switch {
		case !w.open && now.Sub(w.lostAt) >= noLeaderFor:
			w.tell(notify.Raised, now)
			w.open = true
		case w.open && now.Sub(w.toldAt) >= remindEvery:
			w.tell(notify.Reminder, now)
		}
	}
	w.deliver(ctx)
}

// tell queues a message for every channel that takes critical alerts.
// Without channels there is nobody to tell, but the message still counts as
// sent. If a channel has not taken the raised message yet, the all-clear
// drops it, and the channel gets neither.
func (w *QuorumWatch) tell(event string, now time.Time) {
	copied := w.alerts.cluster.SharedCopy()
	w.toldAt = now
	config := copied.Config.Spec
	if config.Alerts == nil {
		return
	}
	message := notify.Message{
		Cluster: copied.ClusterName, Event: event, Key: "no-leader/" + w.host, Kind: "no-leader",
		Severity: clusterspec.SeverityCritical, Subject: "no quorum as seen from " + w.host,
		Text: fmt.Sprintf("For %v or more %s has known no leader: a majority of the voters is out of its reach. "+
			"While there is no leader, no host is fenced, no stack fails over, the leader's alerts are silent and "+
			"the heartbeat stops. This host sends this message itself.", noLeaderFor, w.host),
		Since: w.lostAt,
	}
	for index, channel := range config.Alerts.Channels {
		switch {
		case !clusterspec.AtLeast(message.Severity, channel.MinSeverity):
		case event == notify.Resolved && w.pending[index].Event == notify.Raised:
			delete(w.pending, index)
		default:
			w.pending[index] = message
		}
	}
}

// deliver sends the pending messages.
func (w *QuorumWatch) deliver(ctx context.Context) {
	if len(w.pending) == 0 {
		return
	}
	copied := w.alerts.cluster.SharedCopy()
	notifiers := map[int]notify.Notifier{}
	for _, index := range slices.Sorted(maps.Keys(w.pending)) {
		message := w.pending[index]
		err := w.alerts.send(ctx, copied, notifiers, delivery{channel: index, message: message})
		if err != nil {
			w.alerts.logger.Warn("alert without quorum", "channel", channelName(copied.Config.Spec, index),
				"event", message.Event, "error", err)
			continue
		}
		delete(w.pending, index)
	}
}
