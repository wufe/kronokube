package tui

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

// TestDrillIncludesCrashLoopPodAfterShrink reproduces the scenario reported
// by the user: a StatefulSet has 3 pods, one of them is in CrashLoopBackOff
// throughout the recording, the other two are healthy. After running kk
// shrink on the file, drilling into the StatefulSet must return all three
// pods — the unhealthy one because its blob is intact, the two healthy
// ones because the fallback in resolveResourceBlob looks up any other
// non-shrunk version of them across the file.
//
// This guards against regressions in either the shrink classification or
// the drill ownership resolution.
func TestDrillIncludesCrashLoopPodAfterShrink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drill-test.kk")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const ns = "default"
	const ssName = "demo"
	// Three pods owned by StatefulSet/demo. ss-2 stays unhealthy after a
	// brief Pending startup; ss-0 and ss-1 are healthy after the same
	// brief startup. So every pod has *some* non-shrunk snapshot (the
	// Pending one at least), which matches a realistic recording.
	const snaps = 10
	for i := 0; i < snaps; i++ {
		ts := time.Unix(int64(1700000000+i*30), 0)
		var rows []model.Row
		for _, podName := range []string{"ss-0", "ss-1", "ss-2"} {
			status := "Running"
			ready := "1/1"
			// ss-2 is CrashLoopBackOff from snap 3 onward — the
			// pod-with-incidents case.
			//
			// ss-0 and ss-1 are HEALTHY THROUGHOUT, no Pending startup
			// — they were already running when recording began. This is
			// the regression case: an always-healthy pod must still
			// surface in the drill view because we now preserve one
			// blob per pod regardless of health.
			if podName == "ss-2" && i >= 3 {
				status = "CrashLoopBackOff"
				ready = "0/1"
			}
			rows = append(rows, makePodRow(ns, podName, ssName, status, ready, i))
		}
		statuses := map[model.Kind]model.KindStatus{"pods": model.StatusOK}
		if _, err := st.WriteSnapshot(ts, rows, nil, nil, statuses, nil); err != nil {
			t.Fatalf("write snap %d: %v", i, err)
		}
	}

	snapshots, err := st.ListSnapshots()
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != snaps {
		t.Fatalf("snapshots = %d, want %d", len(snapshots), snaps)
	}

	// Compute shrink targets and run shrink. Mirrors what `kk shrink` does.
	healthRows, err := st.IteratePodHealthRows()
	if err != nil {
		t.Fatalf("iterate health rows: %v", err)
	}
	targets := buildShrinkTargetsForTest(healthRows, snapshots)
	if _, err := st.Shrink(targets, nil); err != nil {
		t.Fatalf("shrink: %v", err)
	}

	// Drill at a snapshot where ss-2 is in CrashLoopBackOff (snap 7) and
	// ss-0 / ss-1 are healthy. The expectation: all three pods appear in
	// the drill set, regardless of how each of them got shrunk.
	snapID := snapshots[7].ID
	got, err := computePodFilter(st, snapID, "statefulsets.apps", ns, ssName)
	if err != nil {
		t.Fatalf("computePodFilter: %v", err)
	}
	for _, want := range []string{"default/ss-0", "default/ss-1", "default/ss-2"} {
		if !got[want] {
			t.Errorf("drill missing pod %q (got=%v)", want, got)
		}
	}
}

// makePodRow constructs a row matching what the snapshotter would write:
// cells in catalog order, raw JSON carrying ownerReferences so drill can
// resolve ownership.
func makePodRow(ns, podName, ssName, status, ready string, snapIdx int) model.Row {
	raw, _ := json.Marshal(map[string]any{
		"kind":       "Pod",
		"apiVersion": "v1",
		"metadata": map[string]any{
			"name":      podName,
			"namespace": ns,
			"uid":       podName + "-uid",
			"ownerReferences": []map[string]any{{
				"apiVersion": "apps/v1",
				"kind":       "StatefulSet",
				"name":       ssName,
				"uid":        ssName + "-uid",
				"controller": true,
			}},
		},
		"spec": map[string]any{
			"nodeName": "node-a",
			"containers": []map[string]any{{
				"name": "app",
			}},
		},
	})
	// Cells layout per defPods: NAMESPACE, NAME, READY, STATUS, RESTARTS, IP, NODE, AGE.
	return model.Row{
		Kind:      "pods",
		Namespace: ns,
		Name:      podName,
		UID:       podName + "-uid",
		Cells:     []string{ns, podName, ready, status, "0", "10.0.0.1", "node-a", "1m"},
		RawJSON:   raw,
	}
}

// buildShrinkTargetsForTest duplicates the logic in cmd/kk's
// computeShrinkTargets without making this test depend on package main.
func buildShrinkTargetsForTest(rows []store.PodHealthRow, snapshots []store.SnapshotInfo) []store.ShrinkTarget {
	snapIdx := make(map[int64]int, len(snapshots))
	for i, s := range snapshots {
		snapIdx[s.ID] = i
	}
	type obs struct {
		i      int
		snapID int64
		health model.PodHealth
	}
	var targets []store.ShrinkTarget
	var cur []obs
	var curNs, curName string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		important := map[int]struct{}{}
		for _, o := range cur {
			if o.health == model.HealthHealthy {
				continue
			}
			important[o.i] = struct{}{}
			if o.i > 0 {
				important[o.i-1] = struct{}{}
			}
			if o.i < len(snapshots)-1 {
				important[o.i+1] = struct{}{}
			}
		}
		// Mirrors the live shrink logic: keep one blob per pod even when
		// the pod was never unhealthy.
		if len(important) == 0 {
			important[cur[0].i] = struct{}{}
		}
		for _, o := range cur {
			if _, keep := important[o.i]; keep {
				continue
			}
			targets = append(targets, store.ShrinkTarget{SnapshotID: o.snapID, Namespace: curNs, Pod: curName})
		}
	}
	for _, r := range rows {
		if r.Namespace != curNs || r.Name != curName {
			flush()
			curNs, curName = r.Namespace, r.Name
			cur = cur[:0]
		}
		i, ok := snapIdx[r.SnapshotID]
		if !ok {
			continue
		}
		cur = append(cur, obs{i: i, snapID: r.SnapshotID, health: model.ClassifyPodHealth(r.Status, r.Ready)})
	}
	flush()
	return targets
}
