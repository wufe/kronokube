package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
)

// ShrinkTarget identifies one (snapshot, pod) pair that should have its
// blob replaced by the shared empty blob and its captured logs deleted.
// Only pod rows are valid targets — the kk shrink command will never ask
// us to strip non-pod resources.
type ShrinkTarget struct {
	SnapshotID int64
	Namespace  string
	Pod        string
}

// ShrinkStats summarizes what Shrink did to the file. Reported by the CLI
// so the user knows whether the operation actually freed space.
type ShrinkStats struct {
	RowsMarked     int
	PodLogsDeleted int
	BlobsBefore    int
	BlobsAfter     int
	BytesBefore    int64
	BytesAfter     int64
}

// ShrinkProgress is an optional callback fired periodically by Shrink so a
// caller can render a progress bar. done <= total within a phase, and the
// phase string changes ("stripping", "finalizing", "vacuuming") as Shrink
// moves through its stages. Safe to leave nil — Shrink skips the calls.
type ShrinkProgress func(done, total int, phase string)

// Shrink marks the given (snapshot, pod) rows as shrunk: their resource
// blob is replaced by a single empty placeholder, their pod_logs entries
// are removed, and any blobs that became orphans are deleted. The file is
// VACUUMed at the end so SQLite actually returns the freed space to the
// filesystem.
//
// Safe to call repeatedly; rows already at shrunk=1 are no-ops, and the
// empty blob is created at most once.
//
// progress may be nil. When set, it is called from the same goroutine that
// runs Shrink, so the callback should be fast (just enqueue a tea.Msg or
// similar — don't block on UI). It's invoked at most once per ~32 rows
// during the per-target loop, and once between phases.
func (s *Store) Shrink(targets []ShrinkTarget, progress ShrinkProgress) (ShrinkStats, error) {
	var stats ShrinkStats
	report := func(done, total int, phase string) {
		if progress != nil {
			progress(done, total, phase)
		}
	}

	// File size before, for a friendly diff in the CLI output.
	if bs, err := fileBytes(s.path); err == nil {
		stats.BytesBefore = bs
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&stats.BlobsBefore); err != nil {
		return stats, err
	}

	if len(targets) == 0 {
		// Still report stats consistently.
		stats.BlobsAfter = stats.BlobsBefore
		if bs, err := fileBytes(s.path); err == nil {
			stats.BytesAfter = bs
		}
		return stats, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return stats, err
	}
	defer func() { _ = tx.Rollback() }()

	// Ensure the shared empty blob exists; reuse it for every shrunk row.
	emptyBlobID, err := upsertEmptyBlob(tx)
	if err != nil {
		return stats, err
	}

	updateRes, err := tx.Prepare(`UPDATE resources
		SET blob_id = ?, shrunk = 1
		WHERE snapshot_id = ? AND kind = 'pods' AND namespace = ? AND name = ? AND shrunk = 0`)
	if err != nil {
		return stats, err
	}
	defer updateRes.Close()

	deleteLog, err := tx.Prepare(`DELETE FROM pod_logs
		WHERE snapshot_id = ? AND namespace = ? AND pod = ?`)
	if err != nil {
		return stats, err
	}
	defer deleteLog.Close()

	total := len(targets)
	report(0, total, "stripping")
	for i, t := range targets {
		r, err := updateRes.Exec(emptyBlobID, t.SnapshotID, t.Namespace, t.Pod)
		if err != nil {
			return stats, fmt.Errorf("shrink %s/%s @snap=%d: %w", t.Namespace, t.Pod, t.SnapshotID, err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			stats.RowsMarked += int(n)
		}
		r2, err := deleteLog.Exec(t.SnapshotID, t.Namespace, t.Pod)
		if err != nil {
			return stats, fmt.Errorf("delete pod_log %s/%s @snap=%d: %w", t.Namespace, t.Pod, t.SnapshotID, err)
		}
		if n, _ := r2.RowsAffected(); n > 0 {
			stats.PodLogsDeleted += int(n)
		}
		// Throttle progress callbacks so we don't drown the consumer in
		// messages. Bumps happen every ~64 rows and always on the last.
		if i&63 == 0 || i == total-1 {
			report(i+1, total, "stripping")
		}
	}

	report(total, total, "finalizing")
	// Garbage-collect blobs that no row references any more. The empty blob
	// itself is referenced by the rows we just shrunk so it stays.
	// NOT EXISTS lets SQLite use the resources_blob_id / pod_logs_blob_id
	// indexes for each candidate blob — without them this scan is
	// O(blobs × refs) and dwarfs everything else on a multi-GB file.
	if _, err := tx.Exec(`DELETE FROM blobs
		WHERE NOT EXISTS (SELECT 1 FROM resources WHERE resources.blob_id = blobs.id)
		  AND NOT EXISTS (SELECT 1 FROM pod_logs WHERE pod_logs.content_blob_id = blobs.id)`); err != nil {
		return stats, fmt.Errorf("gc blobs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return stats, err
	}

	report(total, total, "vacuuming")
	// VACUUM has to run outside the transaction.
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return stats, fmt.Errorf("vacuum: %w", err)
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&stats.BlobsAfter); err != nil {
		return stats, err
	}
	if bs, err := fileBytes(s.path); err == nil {
		stats.BytesAfter = bs
	}
	return stats, nil
}

// upsertEmptyBlob returns the id of the canonical empty blob, inserting
// it on first call.
func upsertEmptyBlob(tx *sql.Tx) (int64, error) {
	empty := []byte{}
	sum := sha256.Sum256(empty)
	h := hex.EncodeToString(sum[:])
	var id int64
	err := tx.QueryRow(`SELECT id FROM blobs WHERE sha256=?`, h).Scan(&id)
	if err == nil {
		return id, nil
	}
	res, err := tx.Exec(`INSERT INTO blobs(sha256, data) VALUES(?,?)`, h, empty)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// fileBytes returns the size of a file on disk, or 0 if it can't be read.
// We use this for friendly "before/after" reporting; failures shouldn't
// abort the shrink.
func fileBytes(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
