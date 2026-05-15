// Package config holds KronoKube's in-memory configuration. There is no
// config file — every setting comes from CLI flags (cmd/kk). This package
// just defines the struct shape, sensible defaults, and a couple of pure
// helpers (namespace inclusion, mode enum).
package config

import (
	"slices"
	"time"
)

// Mode controls which snapshots get persisted.
type Mode string

const (
	// ModeFull persists every captured snapshot. The default.
	ModeFull Mode = "full"
	// ModeIncidentsOnly persists only snapshots that contain at least one
	// unhealthy pod, along with the snapshot immediately before and after
	// each such snapshot. Equivalent to running `kk shrink` continuously
	// while recording.
	ModeIncidentsOnly Mode = "incidents-only"
)

// Config controls what KronoKube captures and how. All fields are populated
// from CLI flags in cmd/kk/main.go; nothing reads or writes a file.
type Config struct {
	// Interval between snapshots. Default 30s.
	Interval time.Duration

	// Mode selects the persistence policy. Default ModeFull.
	Mode Mode

	// Namespaces to include. Empty = all accessible namespaces.
	IncludeNamespaces []string

	// Namespaces to skip. Applied after IncludeNamespaces.
	ExcludeNamespaces []string

	// Kinds is a preset name ("minimal", "default", "workloads", "full") or a
	// comma-separated list of resource kinds (e.g. "pods,services"). Short
	// forms ("deployments" for "deployments.apps") are accepted. Empty means
	// "default".
	Kinds string

	// ExcludeKinds drops kinds from the resolved Kinds set. Same accepted
	// names as Kinds (catalog name or short prefix).
	ExcludeKinds []string

	// Selector is a label selector passed as `-l` to every `kubectl get`.
	// Empty means no selector. Applied uniformly across all captured kinds.
	Selector string

	// Context is the kubeconfig context to bind to. Empty = current-context.
	Context string

	// Kubeconfig path (empty = default ~/.kube/config or $KUBECONFIG).
	Kubeconfig string

	// PodLogs controls capture of a tail of recent log output for every pod
	// in scope. Disabled by default — logs can contain sensitive data and
	// they multiply the kubectl traffic per snapshot.
	PodLogs PodLogsConfig
}

// PodLogsConfig is the toggle for pod log capture.
type PodLogsConfig struct {
	// Enabled turns log capture on. When true, every captured pod gets a
	// `kubectl logs --all-containers --prefix --tail=<TailLines>` fetched
	// after the main snapshot pass.
	Enabled bool
	// TailLines is the per-container tail to fetch. Default 100. The actual
	// per-pod byte size scales with container count.
	TailLines int
	// PerPodTimeout bounds a single `kubectl logs` call so a slow pod can't
	// stall the whole snapshot. Default 5s.
	PerPodTimeout time.Duration
}

// Default returns Config with sensible defaults applied.
func Default() Config {
	return Config{
		Interval: 30 * time.Second,
		Mode:     ModeFull,
		Kinds:    "default",
		PodLogs: PodLogsConfig{
			Enabled:       false,
			TailLines:     100,
			PerPodTimeout: 5 * time.Second,
		},
	}
}

// IsNamespaceCaptured reports whether a namespace passes include/exclude rules.
func (c Config) IsNamespaceCaptured(ns string) bool {
	if len(c.IncludeNamespaces) > 0 && !slices.Contains(c.IncludeNamespaces, ns) {
		return false
	}
	return !slices.Contains(c.ExcludeNamespaces, ns)
}
