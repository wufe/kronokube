package tui

import (
	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

// buildIncidentIndex walks all pod rows across the .kk file and produces a
// per-snapshot severity vector parallel to `snapshots`.
//
// Algorithm:
//
//   - Pods are streamed in (namespace, name, snapshot_id) order so each
//     pod's sequence is contiguous.
//
//   - Per pod we ignore the first observed snapshot (no prior state, so a
//     pod that's born unhealthy never produces an incident — this is how
//     we exclude always-broken pods).
//
//   - On a transition from Healthy → not-Healthy we mark the destination
//     snapshot:
//
//   - destination is SoftBad (Pending, Terminating, ContainerCreating,
//     PodInitializing, Init:*, Unknown) → Yellow.
//
//   - destination is HardBad (CrashLoopBackOff, Error, OOMKilled, etc.,
//     or Running with not-all-ready) → Red if the pod is still HardBad
//     at the NEXT snapshot for that pod (persistence); Yellow if it
//     bounced back (flicker). If there is no next snapshot for this pod
//     because the file ends, treat it as persistent (Red) — at the end
//     of the recording we can't disprove persistence.
//
//   - Severities are unioned across pods per snapshot with a max rule:
//     Red wins over Yellow wins over None.
//
// The whole walk is cheap enough to run inline, but the caller wraps it in
// a goroutine so a large replay file doesn't stall the TUI's first paint.
func buildIncidentIndex(s *store.Store, snapshots []store.SnapshotInfo) ([]model.IncidentSeverity, error) {
	if len(snapshots) == 0 {
		return nil, nil
	}
	rows, err := s.IteratePodHealthRows()
	if err != nil {
		return nil, err
	}

	// Map snapshot ID → index in `snapshots`. Snapshot IDs are dense in
	// practice but we don't rely on it.
	snapIdx := make(map[int64]int, len(snapshots))
	for i, sn := range snapshots {
		snapIdx[sn.ID] = i
	}

	out := make([]model.IncidentSeverity, len(snapshots))

	// Walk pod by pod (contiguous in `rows` thanks to the ORDER BY).
	type sample struct {
		idx    int
		health model.PodHealth
	}
	var cur []sample
	var curKey string

	flush := func() {
		// Need at least two observations to detect a transition.
		if len(cur) < 2 {
			return
		}
		for i := 1; i < len(cur); i++ {
			prev := cur[i-1]
			now := cur[i]
			if prev.health != model.HealthHealthy {
				continue
			}
			if now.health == model.HealthHealthy {
				continue
			}
			var sev model.IncidentSeverity
			switch now.health {
			case model.HealthSoftBad:
				sev = model.IncidentYellow
			case model.HealthHardBad:
				// Persistence: only if the pod is still HardBad at the
				// pod's next observation. Snapshot index gaps (the pod
				// being missing for a snapshot) count as non-persistent.
				persists := false
				if i+1 < len(cur) {
					next := cur[i+1]
					if next.health == model.HealthHardBad && next.idx == now.idx+1 {
						persists = true
					}
				} else {
					// Pod's last observation is also the file's last snapshot.
					// Treat as persistent so end-of-recording failures show red.
					if now.idx == len(out)-1 {
						persists = true
					}
				}
				if persists {
					sev = model.IncidentRed
				} else {
					sev = model.IncidentYellow
				}
			}
			if sev > out[now.idx] {
				out[now.idx] = sev
			}
		}
	}

	for _, r := range rows {
		key := r.Namespace + "\x00" + r.Name
		if key != curKey {
			flush()
			curKey = key
			cur = cur[:0]
		}
		idx, ok := snapIdx[r.SnapshotID]
		if !ok {
			continue
		}
		cur = append(cur, sample{idx: idx, health: model.ClassifyPodHealth(r.Status, r.Ready)})
	}
	flush()

	return out, nil
}
