package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wufe/kronokube/internal/model"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database powering one .kk file.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) a .kk file. Schema is applied if not present.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrateAddShrunkColumn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate resources.shrunk: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.setMeta("schema_version", currentSchemaVersion); err != nil {
		return nil, err
	}
	return s, nil
}

// migrateAddShrunkColumn brings older .kk files up to the current schema
// by adding resources.shrunk if it's missing. CREATE TABLE IF NOT EXISTS
// wouldn't help here — that branch never fires when the table already
// exists, so we use PRAGMA table_info to look for the column.
func migrateAddShrunkColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(resources)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasShrunk := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "shrunk" {
			hasShrunk = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasShrunk {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE resources ADD COLUMN shrunk INTEGER NOT NULL DEFAULT 0`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the file path.
func (s *Store) Path() string { return s.path }

func (s *Store) setMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// GetMeta reads a meta key, returning "" if missing.
func (s *Store) GetMeta(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

// SetClusterInfo records context name and server version into meta.
func (s *Store) SetClusterInfo(contextName, serverVersion string) error {
	if err := s.setMeta("context_name", contextName); err != nil {
		return err
	}
	return s.setMeta("server_version", serverVersion)
}

// --- Writes ---

// WriteSnapshot persists a single tick. Rows/events/logs are grouped by the
// caller; this routine is responsible for blob deduplication.
//
// kindStatus reports per-kind capture status (ok/forbidden/error). It is
// recorded so the TUI can show partial-snapshot honesty.
//
// podLogs may be nil; the field is only populated when pod_logs.enabled.
func (s *Store) WriteSnapshot(ts time.Time, rows []model.Row, events []model.Event, podLogs []model.PodLog, kindStatus map[model.Kind]model.KindStatus, kindErrs map[model.Kind]string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`INSERT INTO snapshots(ts, server_version, context_name) VALUES(?,?,?)`,
		ts.UnixNano(), s.GetMeta("server_version"), s.GetMeta("context_name"))
	if err != nil {
		return 0, fmt.Errorf("insert snapshot: %w", err)
	}
	snapID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for kind, st := range kindStatus {
		if _, err := tx.Exec(`INSERT INTO snapshot_status(snapshot_id,kind,status,error_msg) VALUES(?,?,?,?)`,
			snapID, string(kind), string(st), kindErrs[kind]); err != nil {
			return 0, fmt.Errorf("insert status: %w", err)
		}
	}

	// Cache hash->id within the transaction to avoid repeated SELECTs for
	// already-inserted blobs in this tick.
	blobIDCache := make(map[string]int64, len(rows))
	// Lazily initialized when the first Row.Shrunk arrives. Lets the live
	// `--incidents-only` recorder strip blobs in-flight with the same
	// retention shape that `kk shrink` produces post-hoc.
	var emptyBlobID int64

	for _, r := range rows {
		var blobID int64
		shrunkFlag := 0
		if r.Shrunk {
			if emptyBlobID == 0 {
				emptyBlobID, err = upsertEmptyBlob(tx)
				if err != nil {
					return 0, err
				}
			}
			blobID = emptyBlobID
			shrunkFlag = 1
		} else {
			blobID, err = s.upsertBlob(tx, r.RawJSON, blobIDCache)
			if err != nil {
				return 0, err
			}
		}
		cellsJSON, _ := json.Marshal(r.Cells)
		if _, err := tx.Exec(`INSERT INTO resources(snapshot_id,kind,namespace,name,uid,cells_json,blob_id,shrunk) VALUES(?,?,?,?,?,?,?,?)`,
			snapID, string(r.Kind), r.Namespace, r.Name, r.UID, string(cellsJSON), blobID, shrunkFlag); err != nil {
			return 0, fmt.Errorf("insert resource %s/%s: %w", r.Namespace, r.Name, err)
		}
	}

	for _, e := range events {
		if _, err := tx.Exec(`INSERT INTO events(snapshot_id,namespace,name,last_ts,first_ts,type,reason,object,object_uid,count,message) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			snapID, e.Namespace, e.Name,
			nullableUnix(e.LastTimestamp), nullableUnix(e.FirstTimestamp),
			e.Type, e.Reason, e.Object, e.ObjectUID, e.Count, e.Message); err != nil {
			return 0, fmt.Errorf("insert event: %w", err)
		}
	}

	for _, pl := range podLogs {
		blobID, err := s.upsertBlob(tx, pl.Content, blobIDCache)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO pod_logs(snapshot_id,namespace,pod,tail_lines,bytes,content_blob_id,error_msg) VALUES(?,?,?,?,?,?,?)`,
			snapID, pl.Namespace, pl.Pod, pl.TailLines, len(pl.Content), blobID, pl.ErrorMsg); err != nil {
			return 0, fmt.Errorf("insert pod_log %s/%s: %w", pl.Namespace, pl.Pod, err)
		}
	}

	return snapID, tx.Commit()
}

func (s *Store) upsertBlob(tx *sql.Tx, data []byte, cache map[string]int64) (int64, error) {
	if data == nil {
		data = []byte{}
	}
	sum := sha256.Sum256(data)
	hex := hex.EncodeToString(sum[:])
	if id, ok := cache[hex]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM blobs WHERE sha256=?`, hex).Scan(&id)
	if err == sql.ErrNoRows {
		res, ierr := tx.Exec(`INSERT INTO blobs(sha256,data) VALUES(?,?)`, hex, data)
		if ierr != nil {
			return 0, fmt.Errorf("insert blob: %w", ierr)
		}
		id, ierr = res.LastInsertId()
		if ierr != nil {
			return 0, ierr
		}
	} else if err != nil {
		return 0, fmt.Errorf("lookup blob: %w", err)
	}
	cache[hex] = id
	return id, nil
}

func nullableUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}
