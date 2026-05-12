// Package model defines the data types KronoKube records and replays.
package model

import "time"

// Snapshot is a single moment captured from the cluster.
type Snapshot struct {
	ID        int64
	Timestamp time.Time
	// PerKindStatus records, for each Kind captured this tick, whether the call
	// succeeded, was forbidden by RBAC, or errored out. It is what makes
	// partial captures honest: the user can always see what was missing.
	PerKindStatus map[Kind]KindStatus
}

// KindStatus describes how a particular Kind fared during a snapshot.
type KindStatus string

const (
	StatusOK        KindStatus = "ok"
	StatusForbidden KindStatus = "forbidden"
	StatusError     KindStatus = "error"
	StatusSkipped   KindStatus = "skipped"
)

// Kind is a kubectl-friendly resource identifier (e.g. "pods", "deployments.apps").
type Kind string

// Row is a single tabular row for a resource. Cells correspond to the columns
// declared in the resource catalog for this Kind.
type Row struct {
	Kind      Kind
	Namespace string // "" for cluster-scoped resources
	Name      string
	UID       string
	Cells     []string
	// RawJSON is the resource's full JSON, used for describe/timeline diffing.
	RawJSON []byte
}

// PodLog is the tail of recent log output for a pod, captured when
// config.pod_logs.enabled. One record per pod, with all containers'
// output interleaved and prefixed by kubectl's --prefix flag.
type PodLog struct {
	Namespace string
	Pod       string
	TailLines int
	// Content is the raw bytes returned by `kubectl logs`. May be empty
	// when the pod has produced nothing yet.
	Content []byte
	// ErrorMsg, if non-empty, records why the fetch failed (timeout, RBAC,
	// pod not found, …). Content will be nil in that case.
	ErrorMsg string
}

// Event is a Kubernetes Event captured per snapshot.
type Event struct {
	Namespace      string
	Name           string
	LastTimestamp  time.Time
	FirstTimestamp time.Time
	Type           string // Normal / Warning
	Reason         string
	Object         string // kind/name
	ObjectUID      string
	Count          int32
	Message        string
}
