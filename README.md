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

    # shrink (strip non-essential data for healthy pods)
    kk shrink incident.kk           # in place, prompts for confirmation
    kk shrink incident.kk -o lean.kk -y   # write a copy, no prompt

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
| `--kinds NAME` | Preset or comma-separated kind list (default `default` — see below) |
| `--exclude-kinds k1,k2` | Drop kinds from the resolved set |
| `-l / --selector` | Label selector passed as `-l` to every `kubectl get` |
| `--logs` | Capture a tail of pod logs each snapshot (default 100 lines, 5s timeout) |
| `--logs-tail N` | Per-container tail when `--logs` is on |
| `--logs-timeout D` | Per-pod log-fetch timeout |
| `--config FILE` | YAML config file (see `examples/config.yaml`) |

### Filtering what gets captured

In large production clusters the default capture can take too long because a
handful of high-cardinality kinds dominate the work. Three flags scope it
down — all driven from the command line, no config file required.

`--kinds` accepts a preset name or an explicit comma-separated list. Short
names (`deployments` for `deployments.apps`) are accepted. Presets:

| Preset | Contents |
|---|---|
| `minimal` | pods, deployments, statefulsets, services, events |
| `default` (the default) | every kind in the catalog **except** `endpointslices` and `replicasets` — the two that scale fastest with cluster size |
| `workloads` | pods + the pod-producing controllers (deployments, replicasets, statefulsets, daemonsets, jobs, cronjobs) |
| `full` | every kind in the catalog |

`--exclude-kinds` is applied after `--kinds`, so the common escape hatch on a
huge cluster is just `--kinds full --exclude-kinds endpointslices,replicasets`
(or stick with the default and add more to drop).

`-l` / `--selector` is passed through to every `kubectl get` so the recording
only sees objects whose labels match. Useful for "capture only my app":

    kk record -l app=api,tier!=batch --kinds workloads,events

Unknown kinds and unknown preset names fail at flag-parse time with the valid
set listed in the error.

## TUI controls

    tab / shift-tab     next / prev resource kind
    ↑ ↓ / k j           move row
    PgUp / PgDn         scroll page
    /                   filter
    n                   namespace picker

    ← →                 prev / next snapshot (one at a time)
    ⇧← ⇧→               jump ±10 snapshots
    < >                 jump ±1% of timeline (min 25 snaps)
    , .                 jump to prev / next snapshot with an incident
                        (yellow or red on the timeline). Honors the
                        current drill-down — if you're focused on a
                        StatefulSet, you only land on its pods' incidents.
    Ctrl-A / Ctrl-E     first / last snapshot
    L                   jump to live (resume follow)

    d                   describe selected resource (uses captured data)
    y                   raw YAML of selected resource (from captured data)
    e                   events for selected resource (across whole file)
    t                   change timeline for selected resource
    l                   pod logs at this snapshot (requires pod_logs.enabled)
    enter               drill into a parent (Deployment / StS / DS / RS /
                        Job / CronJob / Node) — show only its pods
    esc                 unwind drill-down / cancel filter / close detail

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
### Shrinking a recording

`kk shrink <file.kk>` strips data that's almost certainly noise for a
post-mortem. For every captured pod we keep its full detail (resource JSON
+ captured logs) only at snapshots where the pod was unhealthy, plus the
two adjacent snapshots (one before and one after, for context). All other
(pod, snapshot) pairs lose their blob — it's replaced by a shared empty
placeholder — and their captured logs are deleted. The pod row itself
stays so the resource keeps appearing in tables, just greyed-out; `d`,
`y`, `l`, `t` on a stripped row show a "no data, run by kk shrink" hint.
Non-pod resources are left untouched. SQLite is `VACUUM`ed at the end so
the freed space is returned to the filesystem.

- **Pod logs** — off by default. Enable with `pod_logs.enabled: true` in
  config (see `examples/config.yaml`); KronoKube then fetches a per-container
  tail (default 100 lines) for every captured pod, using
  `kubectl logs --all-containers --prefix --tail=N`. The snapshotter never
  uses `-f`/`--follow` (the standard allowlist still rejects it) so a stuck
  log can't stall a tick. Logs can contain sensitive data, hence the toggle.

  In the TUI, the pod-logs view shows the captured tail as-is. While
  **recording** and at the head of the timeline (the "LIVE" indicator,
  follow on), pressing **l** instead starts a `kubectl logs -f` stream and
  shows it in place — last 3000 lines are kept in memory (oldest dropped on
  overflow); pausing the timeline reverts to the captured snapshot on the
  next open. Streaming uses a separate, narrowly-scoped validator
  (`kubectl.ValidateStreamingLogs`); `kk safety` prints both validators so
  the carve-out stays auditable. If [loglens](https://github.com/wufe/loglens)
  is on `$PATH`, press **enter** to hand the bytes off — for a captured
  tail loglens gets a static slice; from the live stream it gets a fresh
  `kubectl logs -f` of its own (kk's stream is paused, then resumed when
  loglens exits).

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
