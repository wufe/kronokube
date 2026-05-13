package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wufe/kronokube/internal/model"
)

// SnapshotInfo is a lightweight pointer to a snapshot — used to build the
// timeline scrubber without loading any resource data.
type SnapshotInfo struct {
	ID            int64
	Timestamp     time.Time
	ServerVersion string
	ContextName   string
}

// ListSnapshots returns all snapshot metadata in chronological order.
func (s *Store) ListSnapshots() ([]SnapshotInfo, error) {
	rows, err := s.db.Query(`SELECT id, ts, COALESCE(server_version,''), COALESCE(context_name,'') FROM snapshots ORDER BY ts ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotInfo
	for rows.Next() {
		var info SnapshotInfo
		var tsNano int64
		if err := rows.Scan(&info.ID, &tsNano, &info.ServerVersion, &info.ContextName); err != nil {
			return nil, err
		}
		info.Timestamp = time.Unix(0, tsNano)
		out = append(out, info)
	}
	return out, rows.Err()
}

// SnapshotStatuses returns per-kind capture status for one snapshot.
func (s *Store) SnapshotStatuses(snapID int64) (map[model.Kind]model.KindStatus, map[model.Kind]string, error) {
	rows, err := s.db.Query(`SELECT kind, status, COALESCE(error_msg,'') FROM snapshot_status WHERE snapshot_id=?`, snapID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	st := make(map[model.Kind]model.KindStatus)
	er := make(map[model.Kind]string)
	for rows.Next() {
		var kind, status, errMsg string
		if err := rows.Scan(&kind, &status, &errMsg); err != nil {
			return nil, nil, err
		}
		st[model.Kind(kind)] = model.KindStatus(status)
		if errMsg != "" {
			er[model.Kind(kind)] = errMsg
		}
	}
	return st, er, rows.Err()
}

// ResourcesForKind returns rows for a kind in one snapshot. Cells are decoded
// from the stored JSON. RawJSON is left nil here for cheapness — call FetchRaw
// when you actually need it (describe view).
func (s *Store) ResourcesForKind(snapID int64, kind model.Kind, namespace string) ([]model.Row, error) {
	q := `SELECT namespace, name, uid, cells_json, shrunk FROM resources WHERE snapshot_id=? AND kind=?`
	args := []any{snapID, string(kind)}
	if namespace != "" {
		q += ` AND namespace=?`
		args = append(args, namespace)
	}
	q += ` ORDER BY namespace, name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Row
	for rows.Next() {
		var r model.Row
		var cellsJSON string
		var shrunk int
		if err := rows.Scan(&r.Namespace, &r.Name, &r.UID, &cellsJSON, &shrunk); err != nil {
			return nil, err
		}
		r.Kind = kind
		r.Shrunk = shrunk != 0
		if err := json.Unmarshal([]byte(cellsJSON), &r.Cells); err != nil {
			return nil, fmt.Errorf("decode cells: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FetchAnyRealBlob returns the raw JSON of *any* non-shrunk version of
// the given resource, from anywhere in the file. Used by the drill-down
// fallback when the current snapshot's blob has been stripped by
// `kk shrink`: ownership and node-affinity fields are stable across a
// resource's lifetime, so any real blob can answer the question
// "which parent owns this pod?".
//
// Returns (nil, nil) if no non-shrunk copy exists anywhere in the file.
func (s *Store) FetchAnyRealBlob(kind model.Kind, namespace, name string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT b.data FROM resources r JOIN blobs b ON b.id=r.blob_id
		WHERE r.kind=? AND r.namespace=? AND r.name=? AND r.shrunk=0
		ORDER BY r.snapshot_id ASC LIMIT 1`,
		string(kind), namespace, name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return data, err
}

// FetchRaw returns the raw resource JSON at this snapshot.
func (s *Store) FetchRaw(snapID int64, kind model.Kind, namespace, name string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT b.data FROM resources r JOIN blobs b ON b.id=r.blob_id WHERE r.snapshot_id=? AND r.kind=? AND r.namespace=? AND r.name=?`,
		snapID, string(kind), namespace, name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return data, err
}

// EventsForObject returns all events targeting a specific object UID across
// the .kk file's lifetime, ordered by event lastTimestamp ascending.
func (s *Store) EventsForObject(objectUID string) ([]model.Event, error) {
	rows, err := s.db.Query(`SELECT DISTINCT namespace, name, COALESCE(last_ts,0), COALESCE(first_ts,0), COALESCE(type,''), COALESCE(reason,''), COALESCE(object,''), COALESCE(object_uid,''), COALESCE(count,0), COALESCE(message,'')
		FROM events WHERE object_uid=? ORDER BY COALESCE(last_ts,0) ASC`, objectUID)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// EventsForSnapshot returns all events in a snapshot, optionally namespace-scoped.
func (s *Store) EventsForSnapshot(snapID int64, namespace string) ([]model.Event, error) {
	q := `SELECT namespace, name, COALESCE(last_ts,0), COALESCE(first_ts,0), COALESCE(type,''), COALESCE(reason,''), COALESCE(object,''), COALESCE(object_uid,''), COALESCE(count,0), COALESCE(message,'') FROM events WHERE snapshot_id=?`
	args := []any{snapID}
	if namespace != "" {
		q += ` AND namespace=?`
		args = append(args, namespace)
	}
	q += ` ORDER BY COALESCE(last_ts,0) DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]model.Event, error) {
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var lastTs, firstTs int64
		if err := rows.Scan(&e.Namespace, &e.Name, &lastTs, &firstTs, &e.Type, &e.Reason, &e.Object, &e.ObjectUID, &e.Count, &e.Message); err != nil {
			return nil, err
		}
		if lastTs != 0 {
			e.LastTimestamp = time.Unix(0, lastTs)
		}
		if firstTs != 0 {
			e.FirstTimestamp = time.Unix(0, firstTs)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResourceTimeline returns the list of snapshot IDs in which the resource's
// blob hash changed — i.e. the timestamps at which something visibly changed.
// Returns (snapshotID, ts) pairs in chronological order. Includes the first
// occurrence as a change.
func (s *Store) ResourceTimeline(kind model.Kind, namespace, name string) ([]SnapshotInfo, error) {
	q := `SELECT r.snapshot_id, s.ts, r.blob_id
	      FROM resources r JOIN snapshots s ON s.id=r.snapshot_id
	      WHERE r.kind=? AND r.namespace=? AND r.name=?
	      ORDER BY s.ts ASC`
	rows, err := s.db.Query(q, string(kind), namespace, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotInfo
	var lastBlob int64 = -1
	for rows.Next() {
		var snapID, blobID int64
		var tsNano int64
		if err := rows.Scan(&snapID, &tsNano, &blobID); err != nil {
			return nil, err
		}
		if blobID != lastBlob {
			out = append(out, SnapshotInfo{ID: snapID, Timestamp: time.Unix(0, tsNano)})
			lastBlob = blobID
		}
	}
	return out, rows.Err()
}

// PodHealthRow is a slim projection of the pods table used by the incident
// detector. Status and Ready are pulled out of cells_json (indices baked
// into the catalog at model.defPods).
type PodHealthRow struct {
	SnapshotID int64
	Namespace  string
	Name       string
	Status     string
	Ready      string
}

// IteratePodHealthRows yields every captured pod row in a stable order that
// groups the same (namespace, name) together and orders snapshots within
// each pod chronologically — perfect for the per-pod transition scan.
// Returning a slice (not a callback) keeps the consumer simple; the row
// count is bounded by snapshots × pods per cluster, well within memory.
func (s *Store) IteratePodHealthRows() ([]PodHealthRow, error) {
	rows, err := s.db.Query(`SELECT snapshot_id, namespace, name, cells_json
		FROM resources WHERE kind = 'pods'
		ORDER BY namespace, name, snapshot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PodHealthRow
	for rows.Next() {
		var snapID int64
		var ns, name, cellsJSON string
		if err := rows.Scan(&snapID, &ns, &name, &cellsJSON); err != nil {
			return nil, err
		}
		var cells []string
		if err := json.Unmarshal([]byte(cellsJSON), &cells); err != nil {
			return nil, fmt.Errorf("decode cells for %s/%s: %w", ns, name, err)
		}
		// Pod catalog (see model.defPods): cells = [NAMESPACE, NAME, READY, STATUS, ...]
		var ready, status string
		if len(cells) > 2 {
			ready = cells[2]
		}
		if len(cells) > 3 {
			status = cells[3]
		}
		out = append(out, PodHealthRow{
			SnapshotID: snapID,
			Namespace:  ns,
			Name:       name,
			Status:     status,
			Ready:      ready,
		})
	}
	return out, rows.Err()
}

// PodLogsForSnapshot returns metadata for every pod log captured at snapID.
// Content is left nil — call FetchPodLog to lazy-load the bytes.
func (s *Store) PodLogsForSnapshot(snapID int64, namespace string) ([]model.PodLog, error) {
	q := `SELECT namespace, pod, tail_lines, bytes, COALESCE(error_msg,'') FROM pod_logs WHERE snapshot_id=?`
	args := []any{snapID}
	if namespace != "" {
		q += ` AND namespace=?`
		args = append(args, namespace)
	}
	q += ` ORDER BY namespace, pod`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PodLog
	for rows.Next() {
		var pl model.PodLog
		var bytes int
		if err := rows.Scan(&pl.Namespace, &pl.Pod, &pl.TailLines, &bytes, &pl.ErrorMsg); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// FetchPodLog returns the content bytes for one pod's captured log at snapID.
// Returns (nil, nil) when no record exists. ErrorMsg is non-empty if the
// fetch failed; Content may still be nil.
func (s *Store) FetchPodLog(snapID int64, namespace, pod string) (*model.PodLog, error) {
	var pl model.PodLog
	var bytes int
	err := s.db.QueryRow(`SELECT pl.namespace, pl.pod, pl.tail_lines, pl.bytes, COALESCE(pl.error_msg,''), b.data
		FROM pod_logs pl JOIN blobs b ON b.id=pl.content_blob_id
		WHERE pl.snapshot_id=? AND pl.namespace=? AND pl.pod=?`,
		snapID, namespace, pod).Scan(&pl.Namespace, &pl.Pod, &pl.TailLines, &bytes, &pl.ErrorMsg, &pl.Content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pl, nil
}

// Namespaces returns the distinct set of namespaces seen across the file.
func (s *Store) Namespaces() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT namespace FROM resources WHERE namespace<>'' ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}
