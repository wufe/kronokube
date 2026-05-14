// Package config loads KronoKube's optional YAML configuration.
//
// All settings can also be overridden by CLI flags; the YAML file just makes
// it convenient to keep them in version control next to the project being
// debugged.
package config

import (
	"fmt"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
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

// Config controls what KronoKube captures and how.
type Config struct {
	// Interval between snapshots. Default 30s.
	Interval time.Duration `yaml:"interval"`

	// Mode selects the persistence policy. Default ModeFull.
	Mode Mode `yaml:"mode"`

	// Namespaces to include. Empty = all accessible namespaces.
	IncludeNamespaces []string `yaml:"include_namespaces"`

	// Namespaces to skip. Applied after IncludeNamespaces.
	ExcludeNamespaces []string `yaml:"exclude_namespaces"`

	// Output is the path of the .kk file to write to. May be overridden by --out.
	Output string `yaml:"output"`

	// Context is the kubeconfig context to bind to. Empty = current-context.
	Context string `yaml:"context"`

	// Kubeconfig path (empty = default ~/.kube/config or $KUBECONFIG).
	Kubeconfig string `yaml:"kubeconfig"`

	// PodLogs controls capture of a tail of recent log output for every pod
	// in scope. Disabled by default — logs can contain sensitive data and
	// they multiply the kubectl traffic per snapshot.
	PodLogs PodLogsConfig `yaml:"pod_logs"`
}

// PodLogsConfig is the toggle for pod log capture.
type PodLogsConfig struct {
	// Enabled turns log capture on. When true, every captured pod gets a
	// `kubectl logs --all-containers --prefix --tail=<TailLines>` fetched
	// after the main snapshot pass.
	Enabled bool `yaml:"enabled"`
	// TailLines is the per-container tail to fetch. Default 100. The actual
	// per-pod byte size scales with container count.
	TailLines int `yaml:"tail_lines"`
	// PerPodTimeout bounds a single `kubectl logs` call so a slow pod can't
	// stall the whole snapshot. Default 5s.
	PerPodTimeout time.Duration `yaml:"per_pod_timeout"`
}

// Default returns Config with sensible defaults applied.
func Default() Config {
	return Config{
		Interval: 30 * time.Second,
		Mode:     ModeFull,
		PodLogs: PodLogsConfig{
			Enabled:       false,
			TailLines:     100,
			PerPodTimeout: 5 * time.Second,
		},
	}
}

// Load reads a YAML config file. Missing file is not an error — defaults are
// returned. The caller is responsible for layering CLI flags on top.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	switch c.Mode {
	case "", ModeFull:
		c.Mode = ModeFull
	case ModeIncidentsOnly:
		// ok
	default:
		return c, fmt.Errorf("unknown mode %q (want %q or %q)", c.Mode, ModeFull, ModeIncidentsOnly)
	}
	if c.PodLogs.TailLines <= 0 {
		c.PodLogs.TailLines = 100
	}
	if c.PodLogs.PerPodTimeout <= 0 {
		c.PodLogs.PerPodTimeout = 5 * time.Second
	}
	return c, nil
}

// IsNamespaceCaptured reports whether a namespace passes include/exclude rules.
func (c Config) IsNamespaceCaptured(ns string) bool {
	if len(c.IncludeNamespaces) > 0 && !slices.Contains(c.IncludeNamespaces, ns) {
		return false
	}
	return !slices.Contains(c.ExcludeNamespaces, ns)
}
