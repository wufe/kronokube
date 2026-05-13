package model

import "strings"

// PodHealth is the coarse classification of a pod's status at one snapshot.
// Derived purely from the tabular STATUS and READY cells we already store —
// no JSON re-parse required.
type PodHealth int

const (
	// HealthHealthy: pod is running and (if relevant) all containers ready,
	// or it has terminated cleanly (Succeeded / Completed).
	HealthHealthy PodHealth = iota
	// HealthSoftBad: transient or ambiguous state — Pending, Terminating,
	// PodInitializing, ContainerCreating, Init:*, Unknown. Worth surfacing
	// but doesn't on its own indicate a real failure.
	HealthSoftBad
	// HealthHardBad: concrete failure state — CrashLoopBackOff, Error,
	// ImagePullBackOff, OOMKilled, Evicted, Failed, etc., or a Running pod
	// whose containers aren't all ready (probe failures).
	HealthHardBad
)

// IncidentSeverity is the severity of a *transition* at one snapshot
// (healthy in the previous snap, unhealthy now). Red beats yellow.
type IncidentSeverity int

const (
	IncidentNone IncidentSeverity = iota
	IncidentYellow
	IncidentRed
)

// hardBadStatuses is the set of pod STATUS strings that represent a concrete
// failure. Match is exact (k9s/kubectl conventions).
var hardBadStatuses = map[string]struct{}{
	"CrashLoopBackOff":           {},
	"Error":                      {},
	"Failed":                     {},
	"ImagePullBackOff":           {},
	"ErrImagePull":               {},
	"InvalidImageName":           {},
	"CreateContainerConfigError": {},
	"CreateContainerError":       {},
	"PreCreateHookError":         {},
	"PreStartHookError":          {},
	"PostStartHookError":         {},
	"RunContainerError":          {},
	"ImageInspectError":          {},
	"OOMKilled":                  {},
	"Evicted":                    {},
	"DeadlineExceeded":           {},
	"NodeLost":                   {},
	"ContainerStatusUnknown":     {},
}

// softBadStatuses is the set of transient pod STATUS strings: in normal
// operation pods pass through these on the way to Running, or while being
// torn down. We highlight them in yellow.
var softBadStatuses = map[string]struct{}{
	"Pending":             {},
	"Terminating":         {},
	"ContainerCreating":   {},
	"PodInitializing":     {},
	"Unknown":             {},
	"NotReady":            {},
	"SchedulingDisabled":  {},
	"NodeAffinity":        {},
	"PodScheduled":        {},
	"ContainerStarting":   {},
}

// ClassifyPodHealth maps the STATUS and READY cells (as displayed in the
// k9s-style table) to a PodHealth value. The READY cell is the "n/m" string
// kubectl prints; when STATUS is Running and not every container is ready,
// the pod is hard-bad (probe failure).
func ClassifyPodHealth(status, ready string) PodHealth {
	status = strings.TrimSpace(status)
	ready = strings.TrimSpace(ready)

	// Init:N/M phases are soft-bad — pod is still starting up.
	if strings.HasPrefix(status, "Init:") || strings.HasPrefix(status, "Signal:") {
		return HealthSoftBad
	}
	// Sig:* (signal terminations) is hard-bad.
	if strings.HasPrefix(status, "Sig:") || strings.HasPrefix(status, "ExitCode:") {
		return HealthHardBad
	}
	if _, bad := hardBadStatuses[status]; bad {
		return HealthHardBad
	}
	if _, soft := softBadStatuses[status]; soft {
		return HealthSoftBad
	}
	switch status {
	case "Running":
		if ready != "" && !isReadyComplete(ready) {
			return HealthHardBad
		}
		return HealthHealthy
	case "Succeeded", "Completed":
		return HealthHealthy
	}
	// Unknown / unexpected: be cautious but not alarming.
	if status != "" {
		return HealthSoftBad
	}
	return HealthHealthy
}

// isReadyComplete returns true for "n/n" with n>0, false otherwise (including
// "0/0", which we treat as unhealthy — a pod with no ready containers).
func isReadyComplete(cell string) bool {
	slash := strings.IndexByte(cell, '/')
	if slash <= 0 || slash == len(cell)-1 {
		return false
	}
	a := strings.TrimSpace(cell[:slash])
	b := strings.TrimSpace(cell[slash+1:])
	if a == "" || b == "" || a == "0" || a != b {
		return false
	}
	return true
}
