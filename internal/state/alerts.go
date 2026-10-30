package state

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
)

// Alert is an open alert: a condition the leader saw that someone should
// look at. Only open alerts are in the state; whether and when the leader
// told the channels is in its memory.
type Alert struct {
	// Key is the alert's kind and subject ("pool-use/node0/vg/thin/data"):
	// a condition has one key, so it never has two open alerts.
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // info, warning or critical
	Subject  string `json:"subject"`
	Message  string `json:"message"`
	// Since is when it was raised; a change of severity keeps it.
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

func applyPutAlert(s *State, cmd Command, payload PutAlert) error {
	switch {
	case payload.Key == "" || payload.Kind == "" || payload.Subject == "":
		return errors.New("an alert needs a key, a kind and a subject")
	case !slices.Contains(clusterdir.Severities, payload.Severity):
		return fmt.Errorf("alert %s: severity %q is not info, warning or critical", payload.Key, payload.Severity)
	}
	since := cmd.At
	if open, ok := s.Alerts[payload.Key]; ok {
		since = open.Since
	}
	s.Alerts[payload.Key] = &Alert{
		Key: payload.Key, Kind: payload.Kind, Severity: payload.Severity, Subject: payload.Subject,
		Message: payload.Message, Since: since,
	}
	return nil
}

// ResolveAlert closes an alert whose condition is gone.
type ResolveAlert struct {
	Key string `json:"key"`
}

func applyResolveAlert(s *State, _ Command, payload ResolveAlert) error {
	delete(s.Alerts, payload.Key)
	return nil
}
