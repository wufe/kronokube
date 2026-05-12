package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wufe/kronokube/internal/model"
)

func TestWriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.kk")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SetClusterInfo("test-ctx", "v1.30.0"); err != nil {
		t.Fatalf("SetClusterInfo: %v", err)
	}

	ts := time.Unix(1700000000, 0)
	rows := []model.Row{
		{Kind: "pods", Namespace: "default", Name: "p1", UID: "uid-1",
			Cells: []string{"default", "p1", "1/1", "Running", "0", "10.0.0.1", "node-a", "5m"},
			RawJSON: []byte(`{"kind":"Pod","metadata":{"name":"p1","uid":"uid-1"}}`)},
		{Kind: "pods", Namespace: "default", Name: "p2", UID: "uid-2",
			Cells: []string{"default", "p2", "0/1", "CrashLoopBackOff", "3", "", "node-b", "1m"},
			RawJSON: []byte(`{"kind":"Pod","metadata":{"name":"p2","uid":"uid-2"}}`)},
	}
	statuses := map[model.Kind]model.KindStatus{"pods": model.StatusOK, "events": model.StatusForbidden}
	errs := map[model.Kind]string{"events": "user cannot list events"}

	id, err := s.WriteSnapshot(ts, rows, nil, nil, statuses, errs)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if id <= 0 {
		t.Fatalf("snapshot id = %d, want positive", id)
	}

	snaps, err := s.ListSnapshots()
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListSnapshots: %v len=%d", err, len(snaps))
	}
	if snaps[0].ContextName != "test-ctx" {
		t.Errorf("context_name = %q, want test-ctx", snaps[0].ContextName)
	}
	if !snaps[0].Timestamp.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", snaps[0].Timestamp, ts)
	}

	gotRows, err := s.ResourcesForKind(id, "pods", "")
	if err != nil || len(gotRows) != 2 {
		t.Fatalf("ResourcesForKind: %v len=%d", err, len(gotRows))
	}
	if gotRows[0].Name != "p1" || gotRows[1].Name != "p2" {
		t.Errorf("rows out of order: %+v", gotRows)
	}

	st, em, err := s.SnapshotStatuses(id)
	if err != nil {
		t.Fatalf("SnapshotStatuses: %v", err)
	}
	if st["pods"] != model.StatusOK || st["events"] != model.StatusForbidden {
		t.Errorf("statuses = %v", st)
	}
	if em["events"] == "" {
		t.Errorf("expected event error message preserved")
	}

	raw, err := s.FetchRaw(id, "pods", "default", "p1")
	if err != nil || raw == nil {
		t.Fatalf("FetchRaw: %v raw=%v", err, raw)
	}
}

func TestBlobDeduplication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.kk")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	jsonA := []byte(`{"a":1}`)
	jsonB := []byte(`{"b":2}`)
	mk := func(name string, raw []byte) model.Row {
		return model.Row{Kind: "pods", Namespace: "ns", Name: name, Cells: []string{name}, RawJSON: raw}
	}

	// Two snapshots, same content for p1, different for p2.
	for i := 0; i < 2; i++ {
		_, err := s.WriteSnapshot(time.Now().Add(time.Duration(i)*time.Second),
			[]model.Row{mk("p1", jsonA), mk("p2", jsonB)},
			nil, nil,
			map[model.Kind]model.KindStatus{"pods": model.StatusOK}, nil)
		if err != nil {
			t.Fatalf("WriteSnapshot %d: %v", i, err)
		}
	}

	var blobs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	// Expect 2 unique blobs (jsonA, jsonB) shared across 2 snapshots.
	if blobs != 2 {
		t.Errorf("blobs = %d, want 2", blobs)
	}
}
