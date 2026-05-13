// Package store persists snapshots to a single SQLite file (the .kk file).
//
// The schema is intentionally small. Raw resource JSON is content-addressed
// in a blobs table so unchanged objects don't bloat the file across snapshots.
package store

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             INTEGER NOT NULL,
    server_version TEXT,
    context_name   TEXT
);
CREATE INDEX IF NOT EXISTS snapshots_ts ON snapshots(ts);

CREATE TABLE IF NOT EXISTS snapshot_status (
    snapshot_id INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    status      TEXT NOT NULL,
    error_msg   TEXT,
    PRIMARY KEY (snapshot_id, kind),
    FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS blobs (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256 TEXT UNIQUE NOT NULL,
    data   BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS resources (
    snapshot_id INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    namespace   TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,
    uid         TEXT NOT NULL DEFAULT '',
    cells_json  TEXT NOT NULL,
    blob_id     INTEGER NOT NULL,
    -- shrunk=1 marks rows whose blob has been intentionally stripped by
    -- the kk shrink command. The row stays so the resource keeps
    -- appearing in tables, but its describe / yaml / logs interactions
    -- are disabled in the TUI.
    shrunk      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (snapshot_id, kind, namespace, name),
    FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE,
    FOREIGN KEY (blob_id) REFERENCES blobs(id)
);
CREATE INDEX IF NOT EXISTS resources_uid ON resources(uid);
CREATE INDEX IF NOT EXISTS resources_kind ON resources(kind);
-- Speeds up the orphan-blob GC that kk shrink runs ("delete blobs that
-- no row references"). Without it that step is O(blobs × resources) on a
-- big file and dwarfs the rest of the operation.
CREATE INDEX IF NOT EXISTS resources_blob_id ON resources(blob_id);
-- Used by FetchAnyRealBlob — the drill-down fallback that looks up a
-- non-shrunk version of a (kind, namespace, name) elsewhere in the file
-- when the current snapshot's blob has been stripped. The PRIMARY KEY's
-- leading column is snapshot_id, so it can't serve a cross-snapshot lookup.
CREATE INDEX IF NOT EXISTS resources_locator ON resources(kind, namespace, name);

CREATE TABLE IF NOT EXISTS events (
    snapshot_id INTEGER NOT NULL,
    namespace   TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,
    last_ts     INTEGER,
    first_ts    INTEGER,
    type        TEXT,
    reason      TEXT,
    object      TEXT,
    object_uid  TEXT,
    count       INTEGER,
    message     TEXT,
    PRIMARY KEY (snapshot_id, namespace, name),
    FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS events_object_uid ON events(object_uid);

-- pod_logs stores per-pod log tails captured when config.pod_logs.enabled.
-- content_blob_id reuses the blobs table so identical tails across snapshots
-- (common when nothing's logging) cost one stored copy.
CREATE TABLE IF NOT EXISTS pod_logs (
    snapshot_id     INTEGER NOT NULL,
    namespace       TEXT NOT NULL DEFAULT '',
    pod             TEXT NOT NULL,
    tail_lines      INTEGER NOT NULL,
    bytes           INTEGER NOT NULL,
    content_blob_id INTEGER NOT NULL,
    error_msg       TEXT,
    PRIMARY KEY (snapshot_id, namespace, pod),
    FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE,
    FOREIGN KEY (content_blob_id) REFERENCES blobs(id)
);
-- Same reason as resources_blob_id — speeds up the shrink GC step.
CREATE INDEX IF NOT EXISTS pod_logs_blob_id ON pod_logs(content_blob_id);
`

const currentSchemaVersion = "1"
