package state

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// Alert is an open alert: a condition that the leader saw and that someone
// should look at. The state holds only open alerts. The leader keeps in its
// memory whether and when it told the channels.
type Alert struct {
	// Key is the alert's kind and subject ("pool-use/node0/vg/thin/data").
	// A condition has one key, so it never has two open alerts.
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // info, warning or critical
	Subject  string `json:"subject"`
	Message  string `json:"message"`
	// Since is when the alert was raised. A change of severity does not
	// reset it.
	Since time.Time `json:"since"`
}

// PutAlert raises an alert, or changes the severity or message of one that
// is open.
type PutAlert struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
}

func applyPutAlert(s *State, cmd Command, payload PutAlert) (func(), error) {
	switch {
	case payload.Key == "" || payload.Kind == "" || payload.Subject == "":
		return nil, errors.New("an alert needs a key, a kind and a subject")
	case !slices.Contains(clusterspec.Severities, payload.Severity):
		return nil, fmt.Errorf("alert %s: severity %q is not info, warning or critical", payload.Key, payload.Severity)
	}
	since := cmd.At
	if open, ok := s.Alerts[payload.Key]; ok {
		since = open.Since
	}
	return func() {
		s.Alerts[payload.Key] = &Alert{
			Key: payload.Key, Kind: payload.Kind, Severity: payload.Severity, Subject: payload.Subject,
			Message: payload.Message, Since: since,
		}
	}, nil
}

// ResolveAlert closes an alert whose condition is gone.
type ResolveAlert struct {
	Key string `json:"key"`
}

func applyResolveAlert(s *State, _ Command, payload ResolveAlert) (func(), error) {
	return func() { delete(s.Alerts, payload.Key) }, nil
}
