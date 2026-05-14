package capture

import (
	"testing"

	"github.com/wufe/kronokube/internal/model"
)

// TestShrinkInPlace_PerPodWindow checks the core retention rule: for each
// pod independently, keep the blob iff the pod is unhealthy in this
// snapshot, the previous one, or the next one. The first kept blob for a
// pod also acts as a sentinel.
func TestShrinkInPlace_PerPodWindow(t *testing.T) {
	// Three pods, one snapshot:
	//   pA — healthy here, healthy prev, healthy next  → SHRUNK (after sentinel)
	//   pB — healthy here, unhealthy in prev            → KEPT (after-window)
	//   pC — healthy here, unhealthy in next            → KEPT (before-window)
	//   pD — unhealthy here                             → KEPT (self)
	rows := []model.Row{
		{Kind: "pods", Namespace: "n", Name: "a", UID: "uA", Cells: []string{"n", "a", "1/1", "Running"}, RawJSON: []byte(`{"a":1}`)},
		{Kind: "pods", Namespace: "n", Name: "b", UID: "uB", Cells: []string{"n", "b", "1/1", "Running"}, RawJSON: []byte(`{"b":1}`)},
		{Kind: "pods", Namespace: "n", Name: "c", UID: "uC", Cells: []string{"n", "c", "1/1", "Running"}, RawJSON: []byte(`{"c":1}`)},
		{Kind: "pods", Namespace: "n", Name: "d", UID: "uD", Cells: []string{"n", "d", "0/1", "CrashLoopBackOff"}, RawJSON: []byte(`{"d":1}`)},
	}
	c := &capturedSnap{
		rows:      rows,
		podLogs:   []model.PodLog{{Namespace: "n", Pod: "a"}, {Namespace: "n", Pod: "b"}, {Namespace: "n", Pod: "c"}, {Namespace: "n", Pod: "d"}},
		unhealthy: map[string]bool{"uD": true},
	}
	prev := map[string]bool{"uB": true}
	next := map[string]bool{"uC": true}
	// All four pods already have a sentinel — so the rule isn't masked by
	// the "first observation always kept" carve-out.
	sentinel := map[string]bool{"uA": true, "uB": true, "uC": true, "uD": true}

	shrunk := shrinkInPlace(c, prev, next, sentinel)
	if shrunk != 1 {
		t.Fatalf("shrunk count = %d, want 1", shrunk)
	}

	wantShrunk := map[string]bool{"a": true}
	for _, r := range c.rows {
		if r.Shrunk != wantShrunk[r.Name] {
			t.Errorf("pod %s: Shrunk=%v, want %v", r.Name, r.Shrunk, wantShrunk[r.Name])
		}
	}
	// Logs for the shrunk pod must be dropped; the rest stay.
	wantLogs := map[string]bool{"b": true, "c": true, "d": true}
	got := map[string]bool{}
	for _, pl := range c.podLogs {
		got[pl.Pod] = true
	}
	for name, want := range wantLogs {
		if got[name] != want {
			t.Errorf("pod %s log present=%v, want %v", name, got[name], want)
		}
	}
	if got["a"] {
		t.Errorf("pod a log should have been dropped")
	}
}

// TestShrinkInPlace_Sentinel ensures a pod's first observation is kept
// even when healthy, so drilldown ownerReferences fallback has something
// to find. Subsequent healthy observations may then be shrunk.
func TestShrinkInPlace_Sentinel(t *testing.T) {
	rows := []model.Row{
		{Kind: "pods", Namespace: "n", Name: "p", UID: "uP", Cells: []string{"n", "p", "1/1", "Running"}, RawJSON: []byte(`{"p":1}`)},
	}
	c := &capturedSnap{rows: rows, unhealthy: map[string]bool{}}
	sentinel := map[string]bool{}

	// First observation, all-healthy window — sentinel rule keeps it.
	shrunk := shrinkInPlace(c, nil, nil, sentinel)
	if shrunk != 0 {
		t.Fatalf("first observation: shrunk=%d, want 0", shrunk)
	}
	if c.rows[0].Shrunk {
		t.Fatalf("first observation should not be marked Shrunk")
	}
	if !sentinel["uP"] {
		t.Fatalf("sentinel should be recorded after first kept observation")
	}

	// Second observation, still healthy — now eligible to shrink.
	rows2 := []model.Row{
		{Kind: "pods", Namespace: "n", Name: "p", UID: "uP", Cells: []string{"n", "p", "1/1", "Running"}, RawJSON: []byte(`{"p":2}`)},
	}
	c2 := &capturedSnap{rows: rows2, unhealthy: map[string]bool{}}
	shrunk = shrinkInPlace(c2, nil, nil, sentinel)
	if shrunk != 1 {
		t.Fatalf("second observation: shrunk=%d, want 1", shrunk)
	}
	if !c2.rows[0].Shrunk {
		t.Errorf("second observation should be marked Shrunk")
	}
}

// TestShrinkInPlace_NonPodsUntouched verifies non-pod rows are passed
// through unchanged — `kk shrink` only ever operates on pods, and the
// inline mode must mirror that.
func TestShrinkInPlace_NonPodsUntouched(t *testing.T) {
	rows := []model.Row{
		{Kind: "deployments.apps", Namespace: "n", Name: "d", RawJSON: []byte(`{"d":1}`)},
		{Kind: "services", Namespace: "n", Name: "s", RawJSON: []byte(`{"s":1}`)},
	}
	c := &capturedSnap{rows: rows, unhealthy: map[string]bool{}}
	shrunk := shrinkInPlace(c, nil, nil, map[string]bool{})
	if shrunk != 0 {
		t.Fatalf("non-pod rows should never be shrunk; got %d", shrunk)
	}
	for _, r := range c.rows {
		if r.Shrunk {
			t.Errorf("%s/%s should not be Shrunk", r.Kind, r.Name)
		}
	}
}
