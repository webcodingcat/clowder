// Package compose reads a Compose file with clowder's x-clowder fields, checks
// what clowder can run, and renders the file that docker compose gets on a
// host. It is portable: the CLI checks a stack before sending it.
package compose

import (
	"github.com/compose-spec/compose-go/v2/types"
)

// Stack is a Compose project that passed clowder's checks, with the
// x-clowder fields read into typed values.
type Stack struct {
	Name     string
	Priority Priority
	// Exports are services of this stack that other stacks may reach.
	Exports []string
	// Imports are services of other stacks ("stack/service") this one reaches.
	Imports  []string
	Services map[string]Service
	Volumes  map[string]Volume
	// Project is the Compose model without the x-clowder fields; Render
	// starts from it.
	Project *types.Project
}

// Priority is either a number from 0 to 1000 or the name of a category from
// cluster.yaml; the zero value means "the cluster's default".
type Priority struct {
	Value    *int
	Category string
}

// Service holds what clowder needs to know about one service.
type Service struct {
	Name     string
	Priority Priority
	// HTTP is the route through Traefik, if the service has one.
	HTTP *HTTPRoute
	// Restart is the restart rule from Compose; the agent applies it itself.
	Restart          string
	RestartUnhealthy bool
	DependsOn        []string
	// MemoryReservation and CPUReservation come from deploy.resources; zero
	// means the cluster's default.
	MemoryReservation int64
	CPUReservation    float64
	Ports             []PublishedPort
}

// HTTPRoute sends requests for Host (and Path) to Port of the service.
type HTTPRoute struct {
	Host string
	Port uint32
	Path string
}

// PublishedPort is a TCP or UDP port published on the Floating IP.
type PublishedPort struct {
	Protocol  string
	Published uint16
	Target    uint32
}

// Volume holds the clowder fields of a named volume.
type Volume struct {
	Name string
	// Class is a storage class from cluster.yaml; empty means the cluster's
	// default.
	Class    string
	Size     int64
	Replicas int
	Backup   *Backup
}

// Backup is the backup plan of a volume.
type Backup struct {
	Schedule string
	Keep     int
	Pre      *BackupHook
}

// BackupHook runs a command in a service before the snapshot.
type BackupHook struct {
	Service string
	Command []string
}
