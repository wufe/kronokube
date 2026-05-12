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
}

// KindStat is a per-kind capture outcome.
type KindStat struct {
	Status  model.KindStatus
	Rows    int
	ErrText string
}

// New constructs a Snapshotter. The store and runner must already be ready.
func New(cfg config.Config, runner *kubectl.Runner, s *store.Store) *Snapshotter {
	return &Snapshotter{
		cfg:        cfg,
		runner:     runner,
		store:      s,
		progressCh: make(chan Tick, 8),
	}
}

// Progress returns a channel of per-snapshot progress updates.
func (s *Snapshotter) Progress() <-chan Tick { return s.progressCh }

// Run blocks, capturing snapshots until ctx is cancelled. It calls CaptureOnce
// immediately, then once per cfg.Interval.
func (s *Snapshotter) Run(ctx context.Context) error {
	// Best-effort: record cluster identity at startup.
	cn, _ := s.runner.CurrentContext(ctx)
	sv := s.runner.ServerVersion(ctx)
	_ = s.store.SetClusterInfo(cn, sv)

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

func (s *Snapshotter) publish(t Tick) {
	select {
	case s.progressCh <- t:
	default:
		// TUI is slow; drop. Next tick will overwrite the view anyway.
	}
}

// CaptureOnce runs one full snapshot pass: fan out kubectl gets across the
// catalog, tabulate, write. Failures for individual kinds are recorded as
// per-kind status rather than aborting the snapshot.
func (s *Snapshotter) CaptureOnce(ctx context.Context) Tick {
	ts := time.Now()
	stats := make(map[model.Kind]KindStat, len(model.Catalog))
	statuses := make(map[model.Kind]model.KindStatus, len(model.Catalog))
	errMsgs := make(map[model.Kind]string)

	var allRows []model.Row
	var allEvents []model.Event
	var mu sync.Mutex

	var wg sync.WaitGroup
	// Bound concurrency so we don't fork-bomb kubectl.
	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)

	for _, def := range model.Catalog {
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

	id, werr := s.store.WriteSnapshot(ts, allRows, allEvents, podLogs, statuses, errMsgs)
	return Tick{SnapshotID: id, Timestamp: ts, Stats: stats, Err: werr}
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
