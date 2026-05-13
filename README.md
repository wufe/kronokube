# KronoKube

`kk` — a read-only Kubernetes time machine.

KronoKube periodically takes "snapshots" of your cluster's state by running
read-only `kubectl` commands, and stores them in a single seekable file. Open
that file later in the TUI to scrub through time, see what changed and when,
and debug what was happening at any past moment — without ever having touched
the cluster.

## Safety guarantee

KronoKube never mutates the cluster. It enforces that in code:

- Every `kubectl` invocation goes through one function (`internal/kubectl/runner.go`).
- That function rejects any verb or subcommand not on the allowlist in
  `internal/kubectl/commands.go`.
- A paranoid second pass also rejects any argv containing write-shaped tokens
  (`apply`, `delete`, `patch`, `exec`, `port-forward`, `--force`, …).

Run `kk safety` to print the allowlist and see exactly which command shapes
are accepted versus rejected.

For defense-in-depth, run KronoKube with a read-only ServiceAccount or
kubeconfig.

## Install

With `go install` (drops a `kk` binary in `$GOBIN`, or `$GOPATH/bin` —
make sure that directory is on your `PATH`):

    go install github.com/wufe/kronokube/cmd/kk@latest

Or build from a local checkout:

    go build -o kk ./cmd/kk

### Optional: loglens for log inspection

If [loglens](https://github.com/wufe/loglens) is on your `$PATH`, the pod-logs
view shows an "open in loglens" hint and pressing **enter** suspends `kk`,
hands the captured log bytes to loglens (cursor, search, JSON expand, all of
its keybindings), and returns to `kk` when you quit loglens. KronoKube
doesn't depend on loglens at build time — the feature is detected at runtime
and silently disabled if the binary isn't found.

## Usage

    # record (TUI)
    kk record --out incident.kk --interval 30s

    # record headlessly (cron / systemd)
    kk record --out incident.kk --interval 60s --no-tui

    # replay
    kk replay incident.kk

    # audit safety
    kk safety

Common flags:

| Flag | Meaning |
|---|---|
| `--out FILE` | Output `.kk` file (default `kk-<context>-<date>.kk`) |
| `--interval D` | Snapshot interval (default `30s`) |
| `--context NAME` | kubeconfig context (default: current) |
| `--kubeconfig PATH` | kubeconfig path |
| `--namespace ns1,ns2` | Only capture these namespaces |
| `--exclude-namespace kube-system` | Skip these namespaces |
| `--logs` | Capture a tail of pod logs each snapshot (default 100 lines, 5s timeout) |
| `--logs-tail N` | Per-container tail when `--logs` is on |
| `--logs-timeout D` | Per-pod log-fetch timeout |
| `--config FILE` | YAML config file (see `examples/config.yaml`) |

## TUI controls

    tab / shift-tab     next / prev resource kind
    ↑ ↓ / k j           move row
    PgUp / PgDn         scroll page
    /                   filter
    n                   namespace picker

    ← →                 prev / next snapshot (one at a time)
    ⇧← ⇧→               jump ±10 snapshots
    < >                 jump ±1% of timeline (min 25 snaps)
    Ctrl-A / Ctrl-E     first / last snapshot
    L                   jump to live (resume follow)

    d                   describe selected resource (uses captured data)
    y                   raw YAML of selected resource (from captured data)
    e                   events for selected resource (across whole file)
    t                   change timeline for selected resource
    l                   pod logs at this snapshot (requires pod_logs.enabled)

    ?                   help        Ctrl-C  quit       esc  back

## What is captured

By default, KronoKube captures these read-only views:

- Workloads: pods, deployments, replicasets, statefulsets, daemonsets, jobs, cronjobs
- Networking: services, endpoint slices, ingresses, network policies
- Cluster: nodes, namespaces, HPAs, PDBs, service accounts
- Events

Skipped intentionally:

- **Secrets** — never captured. The `.kk` file is safe to share.
- **ConfigMaps / PVCs / PVs / StorageClasses** — not in default scope.
- **CRDs** — not captured; built-in resources only.
- **Pod logs** — off by default. Enable with `pod_logs.enabled: true` in
  config (see `examples/config.yaml`); KronoKube then fetches a per-container
  tail (default 100 lines) for every captured pod, using
  `kubectl logs --all-containers --prefix --tail=N`. Streaming
  (`-f`/`--follow`) is rejected by the allowlist so a stuck log can't stall
  the snapshot. Logs can contain sensitive data, hence the toggle. In the
  TUI, the pod-logs view shows the captured tail as-is; if
  [loglens](https://github.com/wufe/loglens) is on `$PATH`, press **enter**
  to hand the bytes to loglens for a richer browse session.

When a resource kind is forbidden by RBAC, KronoKube records the denial as
part of the snapshot and continues. Partial captures are honest, not silent.

## File format

A `.kk` file is a single SQLite database. Schema:

- `snapshots(id, ts, server_version, context_name)` — one row per tick.
- `snapshot_status(snapshot_id, kind, status, error_msg)` — per-kind capture outcome.
- `resources(snapshot_id, kind, namespace, name, uid, cells_json, blob_id)` — tabular rows.
- `blobs(id, sha256, data)` — full resource JSON or log content,
  content-addressed so unchanged data doesn't bloat the file across snapshots.
- `events(snapshot_id, namespace, name, last_ts, ..., message)` — events per tick.
- `pod_logs(snapshot_id, namespace, pod, tail_lines, bytes, content_blob_id, error_msg)` — per-pod log tails when `pod_logs.enabled`.

You can inspect a `.kk` file with `sqlite3` if you want to query directly.

## Codebase layout

    cmd/kk/                 CLI entry point
    internal/kubectl/       Single allowlist + exec choke-point (★ safety boundary)
    internal/model/         Resource catalog + column extractors (k9s-style)
    internal/capture/       Snapshotter (ticker loop) + JSON tabulator
    internal/store/         SQLite schema / writer / reader
    internal/config/        YAML config loader
    internal/tui/           Bubble Tea TUI: table, timeline, describe, events
