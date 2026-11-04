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

// noLeaderFor is how long an agent knows no leader before it alerts by
// itself.
const noLeaderFor = 2 * time.Minute

// QuorumWatch alerts from this agent, not the leader, while it knows no
// Raft leader: without a majority of the voters there is none, and the
// leader's alerts are silent then (two hosts of three down, or this host
// cut off). It tells the channels once no leader lasted noLeaderFor,
// reminds every remindEvery and gives the all-clear when a leader is back.
// The channels and their secrets come from this agent's own state, the
// last it applied. What it told is in memory: an agent that restarts
// without a leader tells again.
type QuorumWatch struct {
	alerts *Alerts
	host   string
	// leader tells whether this agent knows a leader (raftnode.Node.Leader)
	leader func() bool

	// Only the watch's loop uses what follows.

	// lostAt is when this agent last stopped knowing a leader; zero while
	// it knows one
	lostAt time.Time
	// open says the channels were told, and toldAt when last
	open   bool
	toldAt time.Time
	// pending is the message each channel, by index, has not taken yet:
	// a newer one replaces it
	pending map[int]notify.Message
}

// NewQuorumWatch is the watch of the agent of host, by its name.
func NewQuorumWatch(alerts *Alerts, host string, leader func() bool) *QuorumWatch {
	return &QuorumWatch{alerts: alerts, host: host, leader: leader, pending: map[int]notify.Message{}}
}

// Run looks every alertsInterval until ctx ends.
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

// round notes whether a leader is known, puts the message that is due for
// each channel, and sends what the channels have not taken yet; a channel
// that fails gets it again next round.
func (w *QuorumWatch) round(ctx context.Context, now time.Time) {
	if w.leader() {
		if w.open {
			w.open = false
			w.tell(notify.Resolved, now)
		}
		w.lostAt = time.Time{}
	} else {
		if w.lostAt.IsZero() {
			w.lostAt = now
		}
		switch {
		case !w.open && now.Sub(w.lostAt) >= noLeaderFor:
			w.open = true
			w.tell(notify.Raised, now)
		case w.open && now.Sub(w.toldAt) >= remindEvery:
			w.tell(notify.Reminder, now)
		}
	}
	w.deliver(ctx)
}

// tell puts a message for every channel that takes critical alerts. A
// channel that has not taken the raised message yet gets no all-clear
// either.
func (w *QuorumWatch) tell(event string, now time.Time) {
	copied, err := w.alerts.cluster.SharedCopy()
	if err != nil {
		w.alerts.logger.Error("alert without quorum", "error", err)
		return
	}
	config := copied.Config.Spec
	if config.Alerts == nil {
		return
	}
	w.toldAt = now
	message := notify.Message{
		Cluster: config.Name, Event: event, Key: "no-leader/" + w.host, Kind: "no-leader",
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
	copied, err := w.alerts.cluster.SharedCopy()
	if err != nil {
		w.alerts.logger.Error("alert without quorum", "error", err)
		return
	}
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
