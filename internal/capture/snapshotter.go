package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wufe/kronokube/internal/config"
	"github.com/wufe/kronokube/internal/kubectl"
	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

// Snapshotter drives the periodic capture loop.
type Snapshotter struct {
	cfg    config.Config
	runner *kubectl.Runner
	store  *store.Store

	// kinds is the ordered list of resource kinds to capture each tick. It's
	// the resolved output of model.ResolveKinds applied to cfg.Kinds /
	// cfg.ExcludeKinds. Catalog entries whose Kind isn't in this set are
	// skipped entirely.
	kinds []model.ResourceDef

	// progressCh receives one Tick per successful (or failed) snapshot. The
	// TUI subscribes to draw the live status. It's a small buffered channel;
	// drop on overflow to avoid blocking the capture loop.
	progressCh chan Tick
}

// Tick is the per-snapshot summary published to the TUI.
type Tick struct {
	SnapshotID int64
	Timestamp  time.Time
	Stats      map[model.Kind]KindStat
	Err        error // overall snapshot-level error, if any
	// Persisted is true when the snapshot's data was written to disk.
	// Currently always true (both modes always write); preserved for
	// callers that want a single field to gate UI updates on.
	Persisted bool
	// HasIncident reports whether the captured snapshot itself contained
	// at least one unhealthy pod.
	HasIncident bool
	// PodsShrunk is the number of pod rows whose blob was replaced by the
	// shared empty placeholder for this snapshot — only ever non-zero in
	// ModeIncidentsOnly. Useful to flash a "shrunk on the fly" indicator.
	PodsShrunk int
}

// capturedSnap holds one snapshot's data in memory before it gets written.
// Used by both modes — the full-mode loop writes immediately, the
// incidents-only loop buffers one snap so per-pod retention can look back
// AND forward by one tick.
type capturedSnap struct {
	ts         time.Time
	rows       []model.Row
	events     []model.Event
	podLogs    []model.PodLog
	kindStatus map[model.Kind]model.KindStatus
	kindErrs   map[model.Kind]string
	stats      map[model.Kind]KindStat
	// unhealthy is the set of pod identities (see podKey) classified as
	// HealthSoftBad or HealthHardBad in this snapshot. len(unhealthy) > 0
	// is the snapshot-level "has incident" predicate.
	unhealthy map[string]bool
}

// KindStat is a per-kind capture outcome.
type KindStat struct {
	Status  model.KindStatus
	Rows    int
	ErrText string
}

// New constructs a Snapshotter. The store and runner must already be ready.
// kinds is the resolved set of resource kinds to capture each tick (see
// model.ResolveKinds). An empty slice falls back to the full catalog so
// existing callers / tests keep working.
func New(cfg config.Config, runner *kubectl.Runner, s *store.Store, kinds []model.Kind) *Snapshotter {
	defs := selectCatalog(kinds)
	return &Snapshotter{
		cfg:        cfg,
		runner:     runner,
		store:      s,
		kinds:      defs,
		progressCh: make(chan Tick, 8),
	}
}

// selectCatalog returns the ResourceDef entries for the given Kind set, in
// catalog order. If kinds is empty, the full catalog is returned (keeps the
// no-filter path unchanged).
func selectCatalog(kinds []model.Kind) []model.ResourceDef {
	if len(kinds) == 0 {
		return model.Catalog
	}
	want := make(map[model.Kind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]model.ResourceDef, 0, len(kinds))
	for _, d := range model.Catalog {
		if want[d.Kind] {
			out = append(out, d)
		}
	}
	return out
}

// Progress returns a channel of per-snapshot progress updates.
func (s *Snapshotter) Progress() <-chan Tick { return s.progressCh }

// Run blocks, capturing snapshots until ctx is cancelled. Dispatches on
// cfg.Mode: ModeFull writes every snapshot; ModeIncidentsOnly buffers one
// snapshot at a time and only persists the (pod-incident ±1) window.
func (s *Snapshotter) Run(ctx context.Context) error {
	// Best-effort: record cluster identity at startup.
	cn, _ := s.runner.CurrentContext(ctx)
	sv := s.runner.ServerVersion(ctx)
	_ = s.store.SetClusterInfo(cn, sv)

	if s.cfg.Mode == config.ModeIncidentsOnly {
		return s.runIncidentsOnly(ctx)
	}
	return s.runFull(ctx)
}

func (s *Snapshotter) runFull(ctx context.Context) error {
	t := s.CaptureOnce(ctx)
	s.publish(t)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			t := s.CaptureOnce(ctx)
			s.publish(t)
		}
	}
}

// runIncidentsOnly buffers one snapshot at a time so per-pod retention
// can look back AND forward by one tick — same window `kk shrink`
// produces post-hoc, but applied inline at record time.
//
// Every snapshot is still written; only the per-pod blob and log are
// stripped when (pod, snapshot) falls outside that pod's incident±1
// window. The snapshot row, the resource row's cells_json, events, and
// non-pod resources are kept as-is so the timeline stays continuous and
// the tabular view always shows what was there.
//
// keptSentinel tracks which pods we've already written at least one full
// blob for. It implements shrink's "always keep one blob per pod" rule:
// without it, an always-healthy pod would lose every blob to the empty
// placeholder and the drill-down's ownerReferences fallback would have
// nothing to fall back to.
func (s *Snapshotter) runIncidentsOnly(ctx context.Context) error {
	var pending *capturedSnap
	var prevUnhealthy map[string]bool // per-pod unhealthy in the snap before pending
	keptSentinel := map[string]bool{}

	flush := func(nextUnhealthy map[string]bool) {
		if pending == nil {
			return
		}
		shrunk := shrinkInPlace(pending, prevUnhealthy, nextUnhealthy, keptSentinel)
		id, err := s.store.WriteSnapshot(pending.ts, pending.rows, pending.events, pending.podLogs, pending.kindStatus, pending.kindErrs)
		s.publish(Tick{
			SnapshotID:  id,
			Timestamp:   pending.ts,
			Stats:       pending.stats,
			Err:         err,
			Persisted:   err == nil && id != 0,
			HasIncident: len(pending.unhealthy) > 0,
			PodsShrunk:  shrunk,
		})
		prevUnhealthy = pending.unhealthy
	}

	step := func() {
		cur := s.captureRaw(ctx)
		flush(cur.unhealthy)
		pending = &cur
	}

	step()

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// No "next" to look at — pass nil so the future side of the
			// window contributes nothing. prev || self still applies.
			flush(nil)
			return ctx.Err()
		case <-ticker.C:
			step()
		}
	}
}

// shrinkInPlace mutates a captured snapshot's pod rows so that any pod
// outside its incident±1 window has Shrunk=true (Store.WriteSnapshot
// turns those into empty-blob references with shrunk=1) and its log
// dropped from podLogs. Returns the number of rows shrunk.
//
// prev / next are per-pod "was unhealthy" maps for the surrounding ticks
// (either may be nil). keptSentinel is mutated so each pod's first kept
// blob marks it as "sentinel satisfied" — subsequent healthy snapshots
// for the same pod can then be safely shrunk.
func shrinkInPlace(c *capturedSnap, prev, next map[string]bool, keptSentinel map[string]bool) int {
	keepLog := map[string]bool{}
	shrunk := 0
	for i, r := range c.rows {
		if r.Kind != "pods" {
			continue
		}
		key := podKey(r)
		keep := prev[key] || c.unhealthy[key] || next[key]
		// Sentinel rule: until a pod has at least one full blob on disk,
		// keep the next observation we'd otherwise shrink. Matches what
		// `kk shrink` does for pods that are always healthy.
		if !keep && !keptSentinel[key] {
			keep = true
		}
		if keep {
			keptSentinel[key] = true
			keepLog[r.Namespace+"/"+r.Name] = true
			continue
		}
		c.rows[i].Shrunk = true
		shrunk++
	}
	if len(c.podLogs) > 0 {
		filtered := c.podLogs[:0]
		for _, pl := range c.podLogs {
			if keepLog[pl.Namespace+"/"+pl.Pod] {
				filtered = append(filtered, pl)
			}
		}
		c.podLogs = filtered
	}
	return shrunk
}

// podKey is the identity we use to track a pod across snapshots. UID is
// the right answer when it's set; namespace/name is a fallback for
// degenerate captures where UID didn't make it into the row.
func podKey(r model.Row) string {
	if r.UID != "" {
		return r.UID
	}
	return r.Namespace + "/" + r.Name
}

func (s *Snapshotter) publish(t Tick) {
	select {
	case s.progressCh <- t:
	default:
		// TUI is slow; drop. Next tick will overwrite the view anyway.
	}
}

// CaptureOnce runs one full snapshot pass and writes it. Used by the
// full-mode loop. Failures for individual kinds are recorded as per-kind
// status rather than aborting the snapshot.
func (s *Snapshotter) CaptureOnce(ctx context.Context) Tick {
	c := s.captureRaw(ctx)
	id, werr := s.store.WriteSnapshot(c.ts, c.rows, c.events, c.podLogs, c.kindStatus, c.kindErrs)
	return Tick{
		SnapshotID:  id,
		Timestamp:   c.ts,
		Stats:       c.stats,
		Err:         werr,
		Persisted:   werr == nil && id != 0,
		HasIncident: len(c.unhealthy) > 0,
	}
}

// captureRaw runs one capture pass and returns the data in memory, without
// touching the store. Used by both CaptureOnce (which persists immediately)
// and the incidents-only loop (which defers the decision by one tick).
func (s *Snapshotter) captureRaw(ctx context.Context) capturedSnap {
	ts := time.Now()
	stats := make(map[model.Kind]KindStat, len(s.kinds))
	statuses := make(map[model.Kind]model.KindStatus, len(s.kinds))
	errMsgs := make(map[model.Kind]string)

	var allRows []model.Row
	var allEvents []model.Event
	var mu sync.Mutex

	var wg sync.WaitGroup
	// Bound concurrency so we don't fork-bomb kubectl.
	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)

	for _, def := range s.kinds {
		wg.Add(1)
		go func(d model.ResourceDef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			raw, err := s.runner.ListResourceJSON(ctx, string(d.Kind), "")
			if err != nil {
				status, msg := classifyErr(err)
				mu.Lock()
				stats[d.Kind] = KindStat{Status: status, ErrText: msg}
				statuses[d.Kind] = status
				errMsgs[d.Kind] = msg
				mu.Unlock()
				return
			}

			rows, terr := Tabulate(d, raw)
			if terr != nil {
				mu.Lock()
				stats[d.Kind] = KindStat{Status: model.StatusError, ErrText: terr.Error()}
				statuses[d.Kind] = model.StatusError
				errMsgs[d.Kind] = terr.Error()
				mu.Unlock()
				return
			}
			rows = s.filterNamespaces(rows, d.Namespaced)

			var events []model.Event
			if d.Kind == "events" {
				events, _ = TabulateEvents(raw)
				events = s.filterEventsNamespaces(events)
			}

			mu.Lock()
			allRows = append(allRows, rows...)
			allEvents = append(allEvents, events...)
			stats[d.Kind] = KindStat{Status: model.StatusOK, Rows: len(rows)}
			statuses[d.Kind] = model.StatusOK
			mu.Unlock()
		}(def)
	}
	wg.Wait()

	// After the structured pass, optionally fetch a tail of recent logs for
	// every captured pod. This is gated behind a config toggle because it
	// multiplies kubectl traffic and the content can be sensitive.
	var podLogs []model.PodLog
	if s.cfg.PodLogs.Enabled {
		podLogs = s.capturePodLogs(ctx, allRows)
	}

	return capturedSnap{
		ts:         ts,
		rows:       allRows,
		events:     allEvents,
		podLogs:    podLogs,
		kindStatus: statuses,
		kindErrs:   errMsgs,
		stats:      stats,
		unhealthy:  perPodUnhealthy(allRows),
	}
}

// perPodUnhealthy returns the set of pod identities (podKey) classified
// as HealthSoftBad or HealthHardBad. Matches the per-(pod, snapshot)
// definition `kk shrink` uses; populated once per tick so the
// incidents-only loop can decide retention per pod, not per snapshot.
//
// Pod cell layout — see model.defPods: [NAMESPACE, NAME, READY, STATUS, …].
func perPodUnhealthy(rows []model.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.Kind != "pods" {
			continue
		}
		if len(r.Cells) < 4 {
			continue
		}
		ready, status := r.Cells[2], r.Cells[3]
		if model.ClassifyPodHealth(status, ready) != model.HealthHealthy {
			out[podKey(r)] = true
		}
	}
	return out
}

// capturePodLogs fan-outs `kubectl logs --tail` for every captured pod.
// Concurrency is bounded; each call has its own short timeout so one slow
// pod can't stall the snapshot.
func (s *Snapshotter) capturePodLogs(parent context.Context, rows []model.Row) []model.PodLog {
	const maxParallel = 6
	sem := make(chan struct{}, maxParallel)

	var wg sync.WaitGroup
	out := make([]model.PodLog, 0)
	var mu sync.Mutex

	tail := s.cfg.PodLogs.TailLines
	if tail <= 0 {
		tail = 100
	}
	timeout := s.cfg.PodLogs.PerPodTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	for _, r := range rows {
		if r.Kind != "pods" {
			continue
		}
		wg.Add(1)
		go func(ns, pod string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(parent, timeout)
			defer cancel()
			content, err := s.runner.Logs(ctx, ns, pod, tail)
			pl := model.PodLog{Namespace: ns, Pod: pod, TailLines: tail}
			if err != nil {
				pl.ErrorMsg = condense(err.Error())
			} else {
				pl.Content = content
			}
			mu.Lock()
			out = append(out, pl)
			mu.Unlock()
		}(r.Namespace, r.Name)
	}
	wg.Wait()
	return out
}

func (s *Snapshotter) filterNamespaces(rows []model.Row, namespaced bool) []model.Row {
	if !namespaced {
		return rows
	}
	if len(s.cfg.IncludeNamespaces) == 0 && len(s.cfg.ExcludeNamespaces) == 0 {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		if s.cfg.IsNamespaceCaptured(r.Namespace) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Snapshotter) filterEventsNamespaces(events []model.Event) []model.Event {
	if len(s.cfg.IncludeNamespaces) == 0 && len(s.cfg.ExcludeNamespaces) == 0 {
		return events
	}
	out := events[:0]
	for _, e := range events {
		if s.cfg.IsNamespaceCaptured(e.Namespace) {
			out = append(out, e)
		}
	}
	return out
}

// classifyErr inspects a kubectl error and returns a coarse status. RBAC
// denials are common and expected — we don't want them to look like crashes.
func classifyErr(err error) (model.KindStatus, string) {
	if err == nil {
		return model.StatusOK, ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case errors.Is(err, kubectl.ErrForbidden):
		// This is the *local* allowlist firing — programming error, not RBAC.
		return model.StatusError, msg
	case strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "cannot list"),
		strings.Contains(lower, "is unable to"):
		return model.StatusForbidden, condense(msg)
	case strings.Contains(lower, "the server could not find the requested resource"),
		strings.Contains(lower, "no matches for kind"),
		strings.Contains(lower, "could not find the requested resource"):
		// API not installed on this cluster — equivalent to "not applicable".
		return model.StatusSkipped, condense(msg)
	default:
		return model.StatusError, condense(msg)
	}
}

// condense keeps error messages short for storage in SQLite.
func condense(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	// Drop newlines so the message renders cleanly in tables.
	msg = strings.ReplaceAll(msg, "\n", " ")
	return fmt.Sprintf("%s", msg)
}
