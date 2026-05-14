// Package tui hosts the KronoKube terminal UI: kind switcher, k9s-style
// resource table, timeline scrubber, describe / events / change-timeline
// panels.
//
// One Model owns all state. Sub-views are pure render functions in sibling
// files (table.go, timeline.go, describe.go).
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wufe/kronokube/internal/capture"
	"github.com/wufe/kronokube/internal/kubectl"
	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

type viewKind int

const (
	viewTable viewKind = iota
	viewDescribe
	viewEvents
	viewChangeTimeline
	viewLogs
	viewYAML
	viewHelp
	viewNamespacePicker
)

// Model is the bubbletea state for the TUI.
type Model struct {
	store    *store.Store
	runner   *kubectl.Runner // non-nil only in live (recording) mode
	keymap   KeyMap
	width    int
	height   int

	// Mode flags
	live     bool                  // live recording in progress
	progress <-chan capture.Tick   // nil in pure-replay mode

	// Snapshot timeline state
	snapshots []store.SnapshotInfo
	curSnap   int  // index into snapshots
	follow    bool // jump to newest on each tick (default true in live mode)

	// drill is non-nil while the user has "entered" a parent resource
	// (Deployment, StatefulSet, Node, …) and is viewing its pods. The pods
	// table is restricted to the keys in drill.podSet; the kind tabs are
	// locked; Esc unwinds back to the originating kind.
	drill *drillState

	// incidents is the per-snapshot severity vector parallel to `snapshots`,
	// max across every captured pod. Built in a goroutine on
	// snapshotsLoadedMsg so opening a 1k-snapshot .kk file doesn't stall
	// the first paint. May lag behind `snapshots` by up to one rebuild;
	// the rebuild is cheap and re-runs on every refresh.
	incidents []model.IncidentSeverity
	// incidentsPerPod is the same data sliced by pod ("namespace/name").
	// Only populated for pods that ever produced an incident. Used when a
	// drill-down is active to narrow the timeline markers to just the
	// selected parent's children.
	incidentsPerPod map[string][]model.IncidentSeverity

	// Resource kind navigation
	kinds   []model.ResourceDef // filtered: excludes Events; Events has its own view
	curKind int

	// Active namespace filter ("" = all)
	namespace string

	// Current snapshot status (per-kind)
	kindStatus map[model.Kind]model.KindStatus
	kindErrs   map[model.Kind]string

	// Rows shown in the table (already filtered/searched).
	rows    []model.Row
	allRows []model.Row // unfiltered (so filter changes are cheap)
	selRow  int
	scroll  int

	// Filter input
	filterInput textinput.Model
	filtering   bool

	// View state
	view          viewKind
	detail        string
	detailScroll  int
	prevView      viewKind
	// logsWrap toggles wrapping inside the logs view. Off by default because
	// raw log output is easier to compare side-by-side with other terminals
	// when it isn't reflowed.
	logsWrap bool
	// logsPL is the captured pod log for the resource currently open in the
	// logs view. Stored as the structured record so renderDetail can call
	// loglens at View time with the live terminal width.
	logsPL *model.PodLog
	// logsTarget is the pod we last loaded logs for. We compare against the
	// currently selected resource to invalidate logsPL on new selections.
	logsTarget string
	// liveLog is non-nil while the logs view is showing a `kubectl logs -f`
	// stream rather than a captured snapshot. Only ever non-nil in live mode
	// when the user opened logs while following the head of the timeline.
	// Torn down on Esc / view switch / quit.
	liveLog *liveLogStream
	// pendingG implements the vim 'gg' two-press jump-to-top sequence inside
	// detail views. Set after a lone 'g'; cleared by the second 'g' (which
	// triggers the jump) or by any other key.
	pendingG bool

	// Namespace picker
	namespaces []string
	nsSel      int
	nsScroll   int

	// Resource-change timeline (when in viewChangeTimeline)
	changeList []store.SnapshotInfo
	changeSel  int

	// Status bar
	statusFlash string
	flashUntil  time.Time

	// Selected resource for describe/events/timeline
	selKind      model.Kind
	selNamespace string
	selName      string
	selUID       string

	contextName string

	// Reference to running snapshotter context for clean shutdown.
	cancel context.CancelFunc
}

// NewModel constructs a TUI model. progress may be nil for pure replay mode.
// runner may be nil in replay mode; it is required for live log streaming
// (used when the user opens logs while following the head of the timeline).
// cancel may be nil; otherwise it is called on Quit.
func NewModel(st *store.Store, live bool, progress <-chan capture.Tick, runner *kubectl.Runner, cancel context.CancelFunc) Model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter (regex-free substring)"
	ti.CharLimit = 200

	m := Model{
		store:       st,
		runner:      runner,
		keymap:      DefaultKeyMap(),
		live:        live,
		progress:    progress,
		follow:      live,
		filterInput: ti,
		contextName: st.GetMeta("context_name"),
		cancel:      cancel,
	}
	// Hide Events from the kind tab list — it has its own dedicated view.
	for _, d := range model.Catalog {
		if d.Kind != "events" {
			m.kinds = append(m.kinds, d)
		}
	}
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		refreshSnapshotsCmd(m.store),
	}
	if m.progress != nil {
		cmds = append(cmds, waitForTickCmd(m.progress))
	}
	cmds = append(cmds, tea.EnterAltScreen)
	cmds = append(cmds, tickClockCmd())
	return tea.Batch(cmds...)
}

// --- bubbletea messages ---

type snapshotsLoadedMsg struct {
	snaps []store.SnapshotInfo
}

type captureTickMsg struct {
	tick capture.Tick
}

type rowsLoadedMsg struct {
	rows       []model.Row
	kindStatus map[model.Kind]model.KindStatus
	kindErrs   map[model.Kind]string
}

type detailLoadedMsg struct {
	view    viewKind
	content string
	events  []model.Event
	changes []store.SnapshotInfo
	// podLog is set when view == viewLogs. We carry the raw bytes through to
	// the model so rendering can happen at View time (loglens output depends
	// on terminal width, which may not be known at load time).
	podLog *model.PodLog
	// podLogTarget is "<ns>/<pod>" so the model can compare against the
	// currently-selected resource and invalidate stale data after navigation.
	podLogTarget string
	// indicatorLine is the 0-based line of a special marker inside content
	// that the viewport should auto-scroll to. -1 = none. Currently used by
	// viewEvents to surface the snapshot-time marker.
	indicatorLine int
}

type namespacesLoadedMsg struct {
	namespaces []string
}

// drillState holds the currently-active drill-down filter.
type drillState struct {
	parentKind model.Kind
	parentNs   string
	parentName string
	// originKind is the index in m.kinds the user was browsing when they
	// drilled in. Esc restores it.
	originKind int
	// originSelRow / originScroll snapshot the table cursor position at
	// the moment of Enter so Esc lands back on the same row instead of
	// resetting to the top.
	originSelRow int
	originScroll int
	// podSet is the filter — keys are "namespace/name". Recomputed on every
	// snapshot change while drill is active.
	podSet map[string]bool
}

// drillLoadedMsg delivers the computed pod set for the active drill at the
// current snapshot. Sent both when the user first presses Enter on a
// parent resource and when the timeline cursor moves to a new snapshot
// while the drill is active.
type drillLoadedMsg struct {
	parentKind model.Kind
	parentNs   string
	parentName string
	originKind int
	// origin{SelRow,Scroll} are -1 on subsequent (snap-change) refreshes,
	// signalling the Update handler not to overwrite the saved cursor.
	originSelRow int
	originScroll int
	podSet       map[string]bool
}

// incidentsLoadedMsg delivers an updated severity vector built in a
// goroutine. The vectors are parallel to model.snapshots at build time;
// applying them when the live timeline has already grown is fine — we
// just see fewer entries than the live count, and the next rebuild fills
// in the new snapshots.
type incidentsLoadedMsg struct {
	global []model.IncidentSeverity
	perPod map[string][]model.IncidentSeverity
}

type clockTickMsg struct{}

type errMsg struct{ err error }

// --- commands ---

func refreshSnapshotsCmd(s *store.Store) tea.Cmd {
	return func() tea.Msg {
		snaps, err := s.ListSnapshots()
		if err != nil {
			return errMsg{err}
		}
		return snapshotsLoadedMsg{snaps}
	}
}

func waitForTickCmd(ch <-chan capture.Tick) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil
		}
		return captureTickMsg{t}
	}
}

func loadRowsCmd(s *store.Store, snapID int64, kind model.Kind, namespace string) tea.Cmd {
	return func() tea.Msg {
		rows, err := s.ResourcesForKind(snapID, kind, namespace)
		if err != nil {
			return errMsg{err}
		}
		ks, errs, _ := s.SnapshotStatuses(snapID)
		return rowsLoadedMsg{rows: rows, kindStatus: ks, kindErrs: errs}
	}
}

func loadDescribeCmd(s *store.Store, snapID int64, kind model.Kind, ns, name, uid string) tea.Cmd {
	return func() tea.Msg {
		raw, err := s.FetchRaw(snapID, kind, ns, name)
		if err != nil {
			return errMsg{err}
		}
		// Events for this snapshot, filtered to this object.
		all, _ := s.EventsForSnapshot(snapID, ns)
		var matched []model.Event
		for _, e := range all {
			if uid != "" && e.ObjectUID == uid {
				matched = append(matched, e)
				continue
			}
			// Fallback by object string "Kind/Name".
			if strings.HasSuffix(e.Object, "/"+name) {
				matched = append(matched, e)
			}
		}
		content := renderDescribe(kind, ns, name, raw, matched)
		return detailLoadedMsg{view: viewDescribe, content: content, events: matched}
	}
}

func loadEventsCmd(s *store.Store, uid string, snapID int64, kind model.Kind, ns, name string, snapTs time.Time) tea.Cmd {
	return func() tea.Msg {
		var evs []model.Event
		if uid != "" {
			evs, _ = s.EventsForObject(uid)
		}
		if len(evs) == 0 {
			all, _ := s.EventsForSnapshot(snapID, ns)
			for _, e := range all {
				if strings.HasSuffix(e.Object, "/"+name) {
					evs = append(evs, e)
				}
			}
		}
		content, indicator := renderEventsList(kind, ns, name, evs, snapTs)
		return detailLoadedMsg{view: viewEvents, events: evs, content: content, indicatorLine: indicator}
	}
}

func loadYAMLCmd(s *store.Store, snapID int64, kind model.Kind, ns, name string) tea.Cmd {
	return func() tea.Msg {
		raw, err := s.FetchRaw(snapID, kind, ns, name)
		if err != nil {
			return errMsg{err}
		}
		return detailLoadedMsg{view: viewYAML, content: renderResourceYAML(kind, ns, name, raw)}
	}
}

func loadLogsCmd(s *store.Store, snapID int64, ns, pod string) tea.Cmd {
	return func() tea.Msg {
		pl, err := s.FetchPodLog(snapID, ns, pod)
		if err != nil {
			return errMsg{err}
		}
		return detailLoadedMsg{
			view:         viewLogs,
			content:      renderPodLogs(ns, pod, pl),
			podLog:       pl,
			podLogTarget: ns + "/" + pod,
		}
	}
}

func loadChangeTimelineCmd(s *store.Store, kind model.Kind, ns, name string) tea.Cmd {
	return func() tea.Msg {
		changes, err := s.ResourceTimeline(kind, ns, name)
		if err != nil {
			return errMsg{err}
		}
		return detailLoadedMsg{view: viewChangeTimeline, changes: changes, content: renderChangeTimeline(kind, ns, name, changes)}
	}
}

// drillDownCmd computes the pod filter for `parentKind/ns/name` at snapID
// in a goroutine and delivers it via drillLoadedMsg. originKind and the
// origin cursor coordinates are carried through so the message handler
// can record where to restore on Esc. Pass originSelRow == -1 for refresh
// (snap-change) calls so the saved cursor isn't overwritten.
func drillDownCmd(s *store.Store, snapID int64, parentKind model.Kind, parentNs, parentName string, originKind, originSelRow, originScroll int) tea.Cmd {
	return func() tea.Msg {
		set, err := computePodFilter(s, snapID, parentKind, parentNs, parentName)
		if err != nil {
			return errMsg{err}
		}
		return drillLoadedMsg{
			parentKind:   parentKind,
			parentNs:     parentNs,
			parentName:   parentName,
			originKind:   originKind,
			originSelRow: originSelRow,
			originScroll: originScroll,
			podSet:       set,
		}
	}
}

// drillRefreshCmd recomputes the pod set for the *current* drill at the
// (now-changed) snapshot ID. Same as drillDownCmd but reuses the existing
// origin info and signals (via -1) that the saved cursor must be kept.
func (m Model) drillRefreshCmd() tea.Cmd {
	if m.drill == nil || len(m.snapshots) == 0 {
		return nil
	}
	snapID := m.snapshots[m.curSnap].ID
	return drillDownCmd(m.store, snapID, m.drill.parentKind, m.drill.parentNs, m.drill.parentName,
		m.drill.originKind, -1, -1)
}

func rebuildIncidentsCmd(s *store.Store, snapshots []store.SnapshotInfo) tea.Cmd {
	// Capture a snapshot slice so the goroutine doesn't race with the
	// model's slice header. The snapshot data itself is immutable per ID.
	snaps := append([]store.SnapshotInfo(nil), snapshots...)
	return func() tea.Msg {
		out, err := buildIncidentIndex(s, snaps)
		if err != nil {
			return errMsg{err}
		}
		return incidentsLoadedMsg{global: out.global, perPod: out.perPod}
	}
}

func loadNamespacesCmd(s *store.Store) tea.Cmd {
	return func() tea.Msg {
		ns, err := s.Namespaces()
		if err != nil {
			return errMsg{err}
		}
		return namespacesLoadedMsg{ns}
	}
}

func tickClockCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return clockTickMsg{} })
}

// --- Update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case clockTickMsg:
		return m, tickClockCmd()

	case snapshotsLoadedMsg:
		prevLen := len(m.snapshots)
		m.snapshots = msg.snaps
		if len(m.snapshots) == 0 {
			return m, nil
		}
		// Follow latest in live mode (or on first load).
		if m.follow || prevLen == 0 {
			m.curSnap = len(m.snapshots) - 1
		} else if m.curSnap >= len(m.snapshots) {
			m.curSnap = len(m.snapshots) - 1
		}
		return m, tea.Batch(m.refreshRowsCmd(), rebuildIncidentsCmd(m.store, m.snapshots))

	case incidentsLoadedMsg:
		m.incidents = msg.global
		m.incidentsPerPod = msg.perPod
		return m, nil

	case drillLoadedMsg:
		// Snap-change refreshes carry originSelRow == -1 — keep the
		// existing saved cursor in that case. Otherwise this is a fresh
		// Enter and we record where the user was before drilling.
		first := m.drill == nil || msg.originSelRow >= 0
		if first {
			m.drill = &drillState{
				parentKind:   msg.parentKind,
				parentNs:     msg.parentNs,
				parentName:   msg.parentName,
				originKind:   msg.originKind,
				originSelRow: msg.originSelRow,
				originScroll: msg.originScroll,
			}
			// Force the table to the Pods kind for the drill view, and
			// reset the cursor so we start at the top of the focused list.
			for i, d := range m.kinds {
				if d.Kind == "pods" {
					m.curKind = i
					break
				}
			}
			m.selRow, m.scroll = 0, 0
		}
		m.drill.podSet = msg.podSet
		return m, m.refreshRowsCmd()

	case captureTickMsg:
		// New snapshot recorded. Reload list, then keep listening.
		cmds := []tea.Cmd{
			refreshSnapshotsCmd(m.store),
			waitForTickCmd(m.progress),
		}
		// Flash a small notice
		st := msg.tick.Stats
		ok := 0
		bad := 0
		for _, s := range st {
			if s.Status == model.StatusOK {
				ok++
			} else if s.Status == model.StatusError {
				bad++
			}
		}
		m.statusFlash = fmt.Sprintf("snap %d • %d kinds ok, %d issues", msg.tick.SnapshotID, ok, bad)
		m.flashUntil = time.Now().Add(3 * time.Second)
		return m, tea.Batch(cmds...)

	case rowsLoadedMsg:
		m.allRows = msg.rows
		m.kindStatus = msg.kindStatus
		m.kindErrs = msg.kindErrs
		m.applyFilter()
		if m.selRow >= len(m.rows) {
			m.selRow = 0
			m.scroll = 0
		}
		return m, nil

	case detailLoadedMsg:
		m.view = msg.view
		m.detail = msg.content
		m.detailScroll = 0
		if msg.view == viewChangeTimeline {
			m.changeList = msg.changes
			m.changeSel = 0
		}
		if msg.view == viewLogs {
			m.logsPL = msg.podLog
			m.logsTarget = msg.podLogTarget
		}
		if msg.indicatorLine >= 0 {
			// Park the marker roughly 1/3 down the viewport so the user
			// sees a bit of "after" context above and the at-snapshot
			// events right below it.
			visible := m.height - 4
			if visible < 5 {
				visible = 5
			}
			target := msg.indicatorLine - visible/3
			if target < 0 {
				target = 0
			}
			m.detailScroll = target
		}
		return m, nil

	case logChunkMsg:
		// Late chunk from a previous pod's stream (user navigated away):
		// drop it, but keep draining the channel so the goroutine can exit.
		if m.liveLog == nil || msg.target != m.liveLog.Target() {
			return m, nil
		}
		if msg.done {
			// Stream ended (kubectl exited or errored). The buffer keeps its
			// final contents; surface the error in the status bar so the
			// user knows the view stopped updating.
			if msg.err != nil {
				m.statusFlash = "logs stream: " + msg.err.Error()
			} else {
				m.statusFlash = "logs stream: closed"
			}
			m.flashUntil = time.Now().Add(5 * time.Second)
			m.detail = renderLiveLogs(m.liveLog.ns, m.liveLog.pod, m.liveLog.Snapshot(), true)
			return m, nil
		}
		m.detail = renderLiveLogs(m.liveLog.ns, m.liveLog.pod, m.liveLog.Snapshot(), false)
		// Keep the latest lines on-screen by snapping the scroll cursor to
		// the last page. Without this, the buffer grows past the visible
		// region and the user sees stale content. detailLastPageStart()
		// accounts for the wrap setting.
		m.detailScroll = m.detailLastPageStart()
		return m, waitForLogChunkCmd(m.liveLog.ch)

	case namespacesLoadedMsg:
		m.namespaces = append([]string{""}, msg.namespaces...) // "" = all
		m.nsSel = 0
		for i, n := range m.namespaces {
			if n == m.namespace {
				m.nsSel = i
				break
			}
		}
		m.view = viewNamespacePicker
		return m, nil

	case errMsg:
		m.statusFlash = "error: " + msg.err.Error()
		m.flashUntil = time.Now().Add(5 * time.Second)
		return m, nil

	case loglensExitedMsg:
		if msg.err != nil {
			m.statusFlash = "loglens: " + msg.err.Error()
			m.flashUntil = time.Now().Add(5 * time.Second)
		}
		// If we suspended our own live stream to hand kubectl off to
		// loglens, restart it now so the view resumes updating in place.
		if m.view == viewLogs && m.live && m.follow && m.runner != nil &&
			m.liveLog == nil && m.selKind == "pods" && m.selName != "" {
			return m.beginLiveLogs(m.selNamespace, m.selName)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) refreshRowsCmd() tea.Cmd {
	if len(m.snapshots) == 0 || m.curKind >= len(m.kinds) {
		return nil
	}
	snap := m.snapshots[m.curSnap]
	return loadRowsCmd(m.store, snap.ID, m.kinds[m.curKind].Kind, m.namespace)
}

func (m *Model) applyFilter() {
	base := m.allRows
	// Drill-down filter takes priority: while drilled, only pods in the
	// computed set are visible. The drill is only meaningful when looking
	// at Pods (we forced curKind there), so this is a no-op for other kinds.
	if m.drill != nil && m.curKind < len(m.kinds) && m.kinds[m.curKind].Kind == "pods" {
		filtered := make([]model.Row, 0, len(base))
		for _, r := range base {
			if m.drill.podSet[r.Namespace+"/"+r.Name] {
				filtered = append(filtered, r)
			}
		}
		base = filtered
	}
	f := strings.ToLower(strings.TrimSpace(m.filterInput.Value()))
	if f == "" {
		m.rows = base
		return
	}
	m.rows = m.rows[:0]
	for _, r := range base {
		hay := strings.ToLower(r.Namespace + " " + r.Name + " " + strings.Join(r.Cells, " "))
		if strings.Contains(hay, f) {
			m.rows = append(m.rows, r)
		}
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keymap

	// Filter input mode swallows most keys.
	if m.filtering {
		switch {
		case key.Matches(msg, k.CancelInput):
			m.filtering = false
			m.filterInput.Blur()
			m.filterInput.SetValue("")
			m.applyFilter()
			return m, nil
		case msg.Type == tea.KeyEnter:
			m.filtering = false
			m.filterInput.Blur()
			m.applyFilter()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.applyFilter()
		return m, cmd
	}

	// Namespace picker is its own little view.
	if m.view == viewNamespacePicker {
		switch {
		case key.Matches(msg, k.CancelInput), key.Matches(msg, k.Back):
			m.view = viewTable
			return m, nil
		case key.Matches(msg, k.Up):
			if m.nsSel > 0 {
				m.nsSel--
			}
			return m, nil
		case key.Matches(msg, k.Down):
			if m.nsSel < len(m.namespaces)-1 {
				m.nsSel++
			}
			return m, nil
		case msg.Type == tea.KeyEnter:
			m.namespace = m.namespaces[m.nsSel]
			m.view = viewTable
			return m, m.refreshRowsCmd()
		}
		return m, nil
	}

	// Help view: any key returns.
	if m.view == viewHelp {
		m.view = viewTable
		return m, nil
	}

	// Detail views (describe/events/timeline/logs/yaml) accept scroll + back.
	if m.view == viewDescribe || m.view == viewEvents || m.view == viewChangeTimeline || m.view == viewLogs || m.view == viewYAML {
		// Vim-style head/tail navigation. 'gg' jumps to the first line; 'G'
		// jumps to the last page. Any other key cancels a pending 'g'.
		raw := msg.String()
		if raw == "g" {
			if m.pendingG {
				m.pendingG = false
				m.detailScroll = 0
				return m, nil
			}
			m.pendingG = true
			return m, nil
		}
		if raw == "G" {
			m.pendingG = false
			m.detailScroll = m.detailLastPageStart()
			return m, nil
		}
		m.pendingG = false

		// Enter in the logs view hands the bytes to loglens (if it's on
		// PATH). For a live stream we tear down our own kubectl process and
		// hand loglens a freshly-started `kubectl logs -f` of its own. For
		// captured snapshots we just pipe the saved bytes.
		if m.view == viewLogs && msg.Type == tea.KeyEnter {
			if m.liveLog != nil {
				ns, pod := m.liveLog.ns, m.liveLog.pod
				snap := m.liveLog.Snapshot()
				m.stopLiveLog()
				cmd, err := spawnLoglensLive(m.runner, ns, pod, snap)
				if err != nil {
					m.statusFlash = "loglens: " + err.Error()
					m.flashUntil = time.Now().Add(3 * time.Second)
					// Resume our own stream so the view keeps updating.
					return m.beginLiveLogs(ns, pod)
				}
				if cmd != nil {
					return m, cmd
				}
				m.statusFlash = "loglens binary not found on PATH"
				m.flashUntil = time.Now().Add(2 * time.Second)
				return m.beginLiveLogs(ns, pod)
			}
			if m.logsPL == nil || len(m.logsPL.Content) == 0 {
				m.statusFlash = "no log bytes to open"
				m.flashUntil = time.Now().Add(2 * time.Second)
				return m, nil
			}
			if cmd := spawnLoglens(m.logsPL.Content); cmd != nil {
				return m, cmd
			}
			m.statusFlash = "loglens binary not found on PATH"
			m.flashUntil = time.Now().Add(2 * time.Second)
			return m, nil
		}

		switch {
		case key.Matches(msg, k.Quit):
			return m.quit()
		case key.Matches(msg, k.Back):
			m.stopLiveLog()
			m.view = viewTable
			return m, nil
		case key.Matches(msg, k.Down):
			m.detailScroll++
			if m.view == viewChangeTimeline && m.changeSel < len(m.changeList)-1 {
				m.changeSel++
			}
			return m, nil
		case key.Matches(msg, k.Up):
			if m.detailScroll > 0 {
				m.detailScroll--
			}
			if m.view == viewChangeTimeline && m.changeSel > 0 {
				m.changeSel--
			}
			return m, nil
		case key.Matches(msg, k.PageDown):
			m.detailScroll += 10
			return m, nil
		case key.Matches(msg, k.PageUp):
			m.detailScroll -= 10
			if m.detailScroll < 0 {
				m.detailScroll = 0
			}
			return m, nil
		case key.Matches(msg, k.Wrap) && m.view == viewLogs:
			m.logsWrap = !m.logsWrap
			// Re-anchor scroll so the same content stays in view after the
			// line-count changes due to wrapping.
			m.detailScroll = 0
			return m, nil
		case msg.Type == tea.KeyEnter && m.view == viewChangeTimeline:
			// Jump to the selected change point in the main timeline.
			if len(m.changeList) > 0 {
				target := m.changeList[m.changeSel].ID
				for i, sn := range m.snapshots {
					if sn.ID == target {
						m.curSnap = i
						m.follow = false
						break
					}
				}
			}
			m.view = viewTable
			return m, m.refreshRowsCmd()
		}
		return m, nil
	}

	// Esc inside the table view exits an active drill-down, restoring the
	// originating kind, cursor position, and scroll offset so the user
	// lands exactly on the parent row they came from. Done here (before
	// the switch) so it takes priority over any other Esc handling.
	if m.drill != nil && msg.Type == tea.KeyEsc {
		d := m.drill
		m.drill = nil
		m.curKind = d.originKind
		m.selRow = d.originSelRow
		m.scroll = d.originScroll
		return m, m.refreshRowsCmd()
	}

	// Enter on a drillable parent (Deployment, StatefulSet, …, Node) shows
	// only its pods. We compute the pod set asynchronously so a slow store
	// doesn't stall the UI. We also capture the current cursor position so
	// Esc can land back on the same parent row.
	if m.drill == nil && msg.Type == tea.KeyEnter {
		if r := m.currentRow(); r != nil && isDrillable(r.Kind) {
			snapID := m.snapshots[m.curSnap].ID
			return m, drillDownCmd(m.store, snapID, r.Kind, r.Namespace, r.Name,
				m.curKind, m.selRow, m.scroll)
		}
	}

	// Main table view bindings.
	switch {
	case key.Matches(msg, k.Quit):
		return m.quit()
	case key.Matches(msg, k.Help):
		m.view = viewHelp
		return m, nil
	case key.Matches(msg, k.Filter):
		m.filtering = true
		m.filterInput.Focus()
		return m, nil
	case key.Matches(msg, k.PrevKind):
		// Kind tabs are locked while a drill is active — the user must Esc
		// out of the focused view first.
		if m.drill != nil {
			m.statusFlash = "kind switch disabled while drilled — esc to unwind"
			m.flashUntil = time.Now().Add(2 * time.Second)
			return m, nil
		}
		if m.curKind > 0 {
			m.curKind--
		} else {
			m.curKind = len(m.kinds) - 1
		}
		m.selRow, m.scroll = 0, 0
		return m, m.refreshRowsCmd()
	case key.Matches(msg, k.NextKind):
		if m.drill != nil {
			m.statusFlash = "kind switch disabled while drilled — esc to unwind"
			m.flashUntil = time.Now().Add(2 * time.Second)
			return m, nil
		}
		m.curKind = (m.curKind + 1) % len(m.kinds)
		m.selRow, m.scroll = 0, 0
		return m, m.refreshRowsCmd()
	case key.Matches(msg, k.PrevSnap):
		return m.jumpSnap(-1)
	case key.Matches(msg, k.NextSnap):
		return m.jumpSnap(+1)
	case key.Matches(msg, k.PrevSnapFast):
		return m.jumpSnap(-10)
	case key.Matches(msg, k.NextSnapFast):
		return m.jumpSnap(+10)
	case key.Matches(msg, k.PrevSnapPage):
		return m.jumpSnap(-m.pageStep())
	case key.Matches(msg, k.NextSnapPage):
		return m.jumpSnap(+m.pageStep())
	case key.Matches(msg, k.PrevSnapIncident):
		return m.jumpToNearestIncident(-1)
	case key.Matches(msg, k.NextSnapIncident):
		return m.jumpToNearestIncident(+1)
	case key.Matches(msg, k.JumpStart):
		m.curSnap = 0
		m.follow = false
		return m, m.refreshRowsCmd()
	case key.Matches(msg, k.JumpEnd), key.Matches(msg, k.Live):
		m.curSnap = max0(len(m.snapshots) - 1)
		m.follow = m.live
		return m, m.refreshRowsCmd()
	case key.Matches(msg, k.Up):
		if m.selRow > 0 {
			m.selRow--
		} else if m.scroll > 0 {
			m.scroll--
		}
		return m, nil
	case key.Matches(msg, k.Down):
		if m.selRow < m.tableVisibleRows()-1 && m.selRow+m.scroll < len(m.rows)-1 {
			m.selRow++
		} else if m.scroll+m.tableVisibleRows() < len(m.rows) {
			m.scroll++
		}
		return m, nil
	case key.Matches(msg, k.PageDown):
		m.scroll += m.tableVisibleRows()
		if m.scroll > len(m.rows)-1 {
			m.scroll = max0(len(m.rows) - 1)
		}
		return m, nil
	case key.Matches(msg, k.PageUp):
		m.scroll -= m.tableVisibleRows()
		if m.scroll < 0 {
			m.scroll = 0
		}
		return m, nil
	case key.Matches(msg, k.Home):
		m.selRow, m.scroll = 0, 0
		return m, nil
	case key.Matches(msg, k.End):
		if len(m.rows) > 0 {
			m.scroll = max0(len(m.rows) - m.tableVisibleRows())
			m.selRow = min(m.tableVisibleRows()-1, len(m.rows)-1-m.scroll)
		}
		return m, nil
	case key.Matches(msg, k.NamespaceSw):
		return m, loadNamespacesCmd(m.store)
	case key.Matches(msg, k.Describe):
		if r := m.currentRow(); r != nil {
			if r.Shrunk {
				m.flashShrunk("describe")
				return m, nil
			}
			m.captureSelection(*r)
			return m, loadDescribeCmd(m.store, m.snapshots[m.curSnap].ID, r.Kind, r.Namespace, r.Name, r.UID)
		}
	case key.Matches(msg, k.Events):
		if r := m.currentRow(); r != nil {
			m.captureSelection(*r)
			return m, loadEventsCmd(m.store, r.UID, m.snapshots[m.curSnap].ID, r.Kind, r.Namespace, r.Name, m.snapshots[m.curSnap].Timestamp)
		}
	case key.Matches(msg, k.Timeline):
		if r := m.currentRow(); r != nil {
			if r.Shrunk {
				m.flashShrunk("change timeline")
				return m, nil
			}
			m.captureSelection(*r)
			return m, loadChangeTimelineCmd(m.store, r.Kind, r.Namespace, r.Name)
		}
	case key.Matches(msg, k.Logs):
		if r := m.currentRow(); r != nil && r.Kind == "pods" {
			if r.Shrunk {
				m.flashShrunk("logs")
				return m, nil
			}
			m.captureSelection(*r)
			// In live+follow mode start a `kubectl logs -f` stream so the
			// user sees output as it arrives. In replay (or while paused
			// mid-history) fall back to the captured snapshot.
			if m.live && m.follow && m.runner != nil {
				return m.beginLiveLogs(r.Namespace, r.Name)
			}
			return m, loadLogsCmd(m.store, m.snapshots[m.curSnap].ID, r.Namespace, r.Name)
		}
		// Non-pod selection: flash a hint rather than silently doing nothing.
		m.statusFlash = "logs view: select a pod"
		m.flashUntil = time.Now().Add(2 * time.Second)
	case key.Matches(msg, k.YAML):
		if r := m.currentRow(); r != nil {
			if r.Shrunk {
				m.flashShrunk("yaml")
				return m, nil
			}
			m.captureSelection(*r)
			return m, loadYAMLCmd(m.store, m.snapshots[m.curSnap].ID, r.Kind, r.Namespace, r.Name)
		}
	}
	return m, nil
}

func (m *Model) captureSelection(r model.Row) {
	m.selKind = r.Kind
	m.selNamespace = r.Namespace
	m.selName = r.Name
	m.selUID = r.UID
	m.prevView = m.view
}

func (m Model) currentRow() *model.Row {
	idx := m.selRow + m.scroll
	if idx < 0 || idx >= len(m.rows) {
		return nil
	}
	return &m.rows[idx]
}

func (m Model) quit() (tea.Model, tea.Cmd) {
	m.stopLiveLog()
	if m.cancel != nil {
		m.cancel()
	}
	return m, tea.Quit
}

// --- View ---

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	switch m.view {
	case viewHelp:
		return m.renderHelp()
	case viewDescribe, viewEvents, viewChangeTimeline, viewLogs, viewYAML:
		return m.renderDetail()
	case viewNamespacePicker:
		return m.renderNamespacePicker()
	}
	return m.renderMain()
}

func (m Model) renderMain() string {
	var b strings.Builder
	// Header line
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	// Kind tabs
	names := make([]string, len(m.kinds))
	for i, d := range m.kinds {
		names[i] = d.DisplayName
	}
	b.WriteString(joinKindTabs(names, m.curKind))
	b.WriteString("\n")
	// Drill banner — visible only while a drill-down is active. Yellow so
	// it can't be missed: the kind tabs are locked and the table content
	// is narrowed, both of which are surprising without a label.
	if m.drill != nil {
		count := len(m.drill.podSet)
		label := fmt.Sprintf(" ▶ pods of %s %s/%s  (%d match%s)  —  esc to clear ",
			drillLabel(m.drill.parentKind), m.drill.parentNs, m.drill.parentName,
			count, plural(count))
		b.WriteString(StyleIncidentYellow.Render(label))
		b.WriteString("\n")
	}
	// Filter line (always rendered for layout stability)
	if m.filtering {
		b.WriteString(m.filterInput.View())
	} else if v := m.filterInput.Value(); v != "" {
		b.WriteString(StyleMuted.Render("filter: " + v))
	}
	b.WriteString("\n")
	// Table
	headers := make([]string, 0, 8)
	for _, c := range m.kinds[m.curKind].Columns {
		headers = append(headers, c.Title)
	}
	rowCells := make([][]string, len(m.rows))
	rowMuted := make([]bool, len(m.rows))
	for i, r := range m.rows {
		rowCells[i] = r.Cells
		rowMuted[i] = r.Shrunk
	}
	cellStyles := m.podCellStyles()
	visible := m.tableVisibleRows()
	b.WriteString(renderTable(headers, rowCells, m.width, m.selRow, m.scroll, visible, rowMuted, cellStyles))
	// Per-kind status note
	if st, ok := m.kindStatus[m.kinds[m.curKind].Kind]; ok && st != model.StatusOK {
		msg := string(st)
		if m.kindErrs[m.kinds[m.curKind].Kind] != "" {
			msg += " — " + m.kindErrs[m.kinds[m.curKind].Kind]
		}
		b.WriteString(StyleWarn.Render(msg))
		b.WriteString("\n")
	}
	// Timeline. The incidents we display narrow to the drilled parent's
	// pods when drill is active, otherwise we show the global view.
	b.WriteString(renderTimelineBar(m.width, m.snapshots, m.curSnap, m.follow || !m.live, m.effectiveIncidents()))
	b.WriteString("\n")
	// Status bar
	b.WriteString(m.renderStatus())
	return b.String()
}

func (m Model) renderHeader() string {
	mode := "REPLAY"
	modeStyle := StyleMuted
	if m.live {
		if m.follow {
			mode = "LIVE"
			modeStyle = StyleOK
		} else {
			mode = fmt.Sprintf("LIVE • paused @ %d", m.curSnap+1)
			modeStyle = StyleWarn
		}
	}
	ctx := m.contextName
	if ctx == "" {
		ctx = "?"
	}
	file := m.store.Path()
	title := StyleTitle.Render("KronoKube")
	parts := []string{
		title,
		modeStyle.Render("[" + mode + "]"),
		StyleMuted.Render("ctx: " + ctx),
		StyleMuted.Render("ns: " + nsLabel(m.namespace)),
		StyleMuted.Render("file: " + shortFile(file)),
	}
	// Time indicator: wall clock when actively following live; otherwise the
	// timestamp of the snapshot we're displaying, so the header agrees with
	// the data on screen.
	if m.live && m.follow {
		parts = append(parts, StyleMuted.Render(time.Now().Format("15:04:05")))
	} else if len(m.snapshots) > 0 && m.curSnap < len(m.snapshots) {
		parts = append(parts, StyleMuted.Render("at: "+m.snapshots[m.curSnap].Timestamp.Format("15:04:05")))
	}
	return strings.Join(parts, "  ")
}

func nsLabel(s string) string {
	if s == "" {
		return "<all>"
	}
	return s
}

func shortFile(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (m Model) renderStatus() string {
	help := "tab: kind  /: filter  enter: drill  d: describe  y: yaml  e: events  t: changes  l: logs  n: ns  ←/→: 1  ⇧←/⇧→: 10  </>: 1%  ,/.: incident  L: live  ?: help  C-c: quit"
	if time.Now().Before(m.flashUntil) && m.statusFlash != "" {
		return StyleOK.Render(m.statusFlash) + "  " + StyleMuted.Render(help)
	}
	return StyleStatusBar.Render(help)
}

func (m Model) tableVisibleRows() int {
	// Header(1) + tabs(1) + filter(1) + table-header(1) + warn(0/1) + tl-bar(2) + status(1).
	reserved := 8
	if m.drill != nil {
		// Drill banner consumes one extra row above the filter line.
		reserved++
	}
	v := m.height - reserved
	if v < 5 {
		v = 5
	}
	return v
}

func (m Model) renderDetail() string {
	title := ""
	switch m.view {
	case viewDescribe:
		title = "Describe"
	case viewEvents:
		title = "Events"
	case viewChangeTimeline:
		title = "Changes Timeline"
	case viewLogs:
		title = "Pod Logs"
	case viewYAML:
		title = "YAML"
	}
	header := StyleTitle.Render(fmt.Sprintf("%s — %s/%s", title, m.selNamespace, m.selName))
	body := m.detail
	// Hard-wrap log content when the user has toggled wrap on. We pre-wrap
	// before splitting so the scroll/page math below counts visual lines.
	if m.view == viewLogs && m.logsWrap && m.width > 0 {
		body = hardWrap(body, m.width)
	}
	lines := strings.Split(body, "\n")
	visible := m.height - 4
	if visible < 5 {
		visible = 5
	}
	maxStart := max0(len(lines) - visible)
	start := m.detailScroll
	if start > maxStart {
		start = maxStart
	}
	end := start + visible
	if end > len(lines) {
		end = len(lines)
	}

	if m.view == viewChangeTimeline {
		// Render with selection
		var b strings.Builder
		b.WriteString(header + "\n\n")
		for i := start; i < end; i++ {
			line := lines[i]
			// rough heuristic: lines with "─" are decorative; first lines are content
			if i-2 == m.changeSel && i >= 2 {
				b.WriteString(StyleSelected.Render(line))
			} else {
				b.WriteString(line)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(StyleMuted.Render("enter: jump to this snapshot   esc: back   C-c: quit"))
		return b.String()
	}

	body = strings.Join(lines[start:end], "\n")
	footerText := fmt.Sprintf("line %d/%d   ↑↓ scroll   gg/G: top/bottom   esc: back   C-c: quit", start+1, len(lines))
	if m.view == viewLogs {
		wrapState := "off"
		if m.logsWrap {
			wrapState = "on"
		}
		ll := ""
		if loglensPath() != "" {
			ll = "   enter: open in loglens"
		}
		footerText = fmt.Sprintf("line %d/%d   ↑↓ scroll   gg/G: top/bottom   w: wrap (%s)%s   esc: back   C-c: quit", start+1, len(lines), wrapState, ll)
	}
	footer := StyleMuted.Render(footerText)
	return header + "\n\n" + body + "\n" + footer
}

// hardWrap splits any line in s that's longer than width into chunks of
// width runes. Existing newlines are preserved. The result has no trailing
// newline. Plain rune slicing — fine for raw log output (no ANSI escapes
// to worry about coming from kubectl --prefix --tail).
func hardWrap(s string, width int) string {
	if width < 1 {
		return s
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		r := []rune(line)
		for len(r) > width {
			b.WriteString(string(r[:width]))
			b.WriteByte('\n')
			r = r[width:]
		}
		b.WriteString(string(r))
	}
	return b.String()
}

func (m Model) renderNamespacePicker() string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render("Namespace") + "\n\n")
	visible := m.height - 4
	if visible < 5 {
		visible = 5
	}
	start := 0
	if m.nsSel >= visible {
		start = m.nsSel - visible + 1
	}
	end := start + visible
	if end > len(m.namespaces) {
		end = len(m.namespaces)
	}
	for i := start; i < end; i++ {
		label := m.namespaces[i]
		if label == "" {
			label = "<all namespaces>"
		}
		if i == m.nsSel {
			b.WriteString(StyleSelected.Render("  " + label + "  "))
		} else {
			b.WriteString("  " + label)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(StyleMuted.Render("↑↓: choose   enter: apply   esc: cancel"))
	return b.String()
}

func (m Model) renderHelp() string {
	help := `KronoKube — read-only Kubernetes time machine

NAVIGATION
  tab / shift-tab   next / prev resource kind
  ↑ ↓ / k j         move row
  PgUp / PgDn       scroll page
  g / G             top / bottom
  /                 filter
  n                 namespace picker

TIMELINE (REPLAY)
  ← →               prev / next snapshot (one at a time)
  ⇧← ⇧→             jump ±10 snapshots
  < >               jump ±1% of timeline (min 25 snaps)
  , .               jump to prev / next snapshot with an incident
  ctrl-a / ctrl-e   first / last snapshot
  L                 jump to live (resume follow)

INSPECT
  d                 describe selected resource
  y                 raw YAML of selected resource (from captured data)
  e                 events for selected resource (across all snapshots)
  t                 changes timeline for selected resource
  l                 pod logs at this snapshot (when pod_logs.enabled in config)
  enter             drill into a parent (Deployment / StS / DS / RS / Job /
                    CronJob / Node) — shows just its pods; esc unwinds
  w                 toggle line wrap inside the logs view
  enter             open the current log tail in loglens (if loglens is on PATH)
  gg / G            (inside detail views) jump to first / last line

OTHER
  ?                 help
  ctrl-c            quit
  esc               back / cancel

SAFETY
  KronoKube only ever runs the kubectl commands declared in
  internal/kubectl/commands.go. Every invocation is checked at
  runtime; anything not on the allowlist is rejected before exec.
`
	return help + "\n" + StyleMuted.Render("press any key to return")
}

// renderEventsList is used as the "content" payload of detailLoadedMsg for the
// events view. It is a top-level function (not method) so it sits next to its
// peers in describe.go but doesn't need Model state.
// renderEventsList returns the rendered content plus the 0-based line index
// of the snapshot-time marker (or -1 if no marker was drawn). The marker
// separates events with LastTimestamp > snapTs (above, "after snapshot")
// from those at-or-before snapTs (below). Sort is newest-first.
func renderEventsList(kind model.Kind, ns, name string, events []model.Event, snapTs time.Time) (string, int) {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("Events for %s %s/%s — %d total", kind, ns, name, len(events))))
	b.WriteString("\n\n")
	if len(events) == 0 {
		b.WriteString(StyleMuted.Render("<none captured>"))
		return b.String(), -1
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].LastTimestamp.After(events[j].LastTimestamp)
	})

	markerLine := -1
	markerEmitted := snapTs.IsZero()
	emitMarker := func() {
		markerLine = strings.Count(b.String(), "\n")
		fmt.Fprintf(&b, "%s\n\n", StyleTimeline.Render(fmt.Sprintf("─── snapshot @ %s ───", snapTs.Format(time.RFC3339))))
		markerEmitted = true
	}

	for _, e := range events {
		if !markerEmitted && !e.LastTimestamp.After(snapTs) {
			emitMarker()
		}
		typ := e.Type
		switch typ {
		case "Warning":
			typ = StyleWarn.Render(typ)
		case "Normal":
			typ = StyleOK.Render(typ)
		}
		ts := "-"
		if !e.LastTimestamp.IsZero() {
			ts = e.LastTimestamp.Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "  %s  %s  %s  ×%d\n", ts, typ, e.Reason, e.Count)
		fmt.Fprintf(&b, "    %s\n\n", e.Message)
	}
	if !markerEmitted {
		emitMarker()
	}
	return b.String(), markerLine
}

// renderChangeTimeline lists snapshots in which this resource visibly changed.
func renderChangeTimeline(kind model.Kind, ns, name string, changes []store.SnapshotInfo) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("Changes — %s %s/%s — %d", kind, ns, name, len(changes))))
	b.WriteString("\n\n")
	if len(changes) == 0 {
		b.WriteString(StyleMuted.Render("<no changes recorded — resource may not exist in this file>"))
		return b.String()
	}
	for i, c := range changes {
		label := c.Timestamp.Format("2006-01-02 15:04:05")
		marker := " "
		if i == 0 {
			marker = "▶" // first seen
		}
		fmt.Fprintf(&b, " %s  #%d  %s\n", marker, c.ID, label)
	}
	return b.String()
}

// detailLastPageStart computes the scroll offset that puts the last line of
// the detail body at the bottom of the viewport. Needs to take wrap state
// into account since wrapping changes line count.
func (m Model) detailLastPageStart() int {
	body := m.detail
	if m.view == viewLogs && m.logsWrap && m.width > 0 {
		body = hardWrap(body, m.width)
	}
	total := strings.Count(body, "\n") + 1
	visible := m.height - 4
	if visible < 5 {
		visible = 5
	}
	if total <= visible {
		return 0
	}
	return total - visible
}

// beginLiveLogs starts a `kubectl logs -f` stream for ns/pod, parks the
// model on viewLogs, and arms the chunk-wait command that drives the live
// updates. Any existing stream is torn down first so we never have two
// kubectl processes against the same view.
func (m Model) beginLiveLogs(ns, pod string) (tea.Model, tea.Cmd) {
	m.stopLiveLog()
	ls, err := startLiveLogStream(m.runner, ns, pod)
	if err != nil {
		m.statusFlash = "live logs: " + err.Error()
		m.flashUntil = time.Now().Add(5 * time.Second)
		// Fall back to captured snapshot so the keypress isn't lost.
		return m, loadLogsCmd(m.store, m.snapshots[m.curSnap].ID, ns, pod)
	}
	m.liveLog = ls
	m.logsPL = nil
	m.logsTarget = ns + "/" + pod
	m.view = viewLogs
	m.detailScroll = 0
	m.detail = renderLiveLogs(ns, pod, nil, false)
	return m, waitForLogChunkCmd(ls.ch)
}

// stopLiveLog cancels the kubectl streaming process (if any). Safe to call
// when no stream is active.
func (m *Model) stopLiveLog() {
	if m.liveLog == nil {
		return
	}
	m.liveLog.Stop()
	m.liveLog = nil
}

// renderLiveLogs formats a streaming buffer for the scrollable detail view.
// `closed` is set after the kubectl process exits so we can mark the buffer
// as final instead of "still updating".
func renderLiveLogs(ns, pod string, content []byte, closed bool) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("Logs (live) — %s/%s", ns, pod)))
	b.WriteString("\n\n")
	tag := "(streaming — last 3000 lines kept in memory)"
	if closed {
		tag = "(stream closed)"
	}
	b.WriteString(StyleMuted.Render(tag))
	b.WriteString("\n\n")
	if len(content) == 0 {
		b.WriteString(StyleMuted.Render("<waiting for output…>"))
		return b.String()
	}
	b.Write(content)
	return b.String()
}

// renderPodLogs formats the captured log tail as a self-contained string
// suitable for the scrollable detail view. The raw bytes are kept on the
// model separately (m.logsPL) so the "open in loglens" subprocess path can
// pipe them without re-fetching from the store.
func renderPodLogs(ns, pod string, pl *model.PodLog) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("Logs — %s/%s", ns, pod)))
	b.WriteString("\n\n")
	if pl == nil {
		b.WriteString(StyleMuted.Render("No logs captured for this pod at this snapshot."))
		b.WriteString("\n\n")
		b.WriteString(StyleMuted.Render("Enable in config:") + "\n")
		b.WriteString("  pod_logs:\n    enabled: true\n    tail_lines: 100\n")
		return b.String()
	}
	if pl.ErrorMsg != "" {
		b.WriteString(StyleWarn.Render("capture error: " + pl.ErrorMsg))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "%s tail_lines=%d   bytes=%d\n\n",
		StyleMuted.Render("(per container, prefixed by [pod/container])"),
		pl.TailLines, len(pl.Content))
	if len(pl.Content) == 0 {
		b.WriteString(StyleMuted.Render("<no output>"))
		return b.String()
	}
	b.Write(pl.Content)
	return b.String()
}

// jumpSnap moves the timeline cursor by delta snapshots, clamps to range,
// and toggles follow appropriately. Centralized so 1-step, 10-step, and
// page-step navigation all behave identically.
func (m Model) jumpSnap(delta int) (tea.Model, tea.Cmd) {
	if len(m.snapshots) == 0 || delta == 0 {
		return m, nil
	}
	target := m.curSnap + delta
	if target < 0 {
		target = 0
	}
	if target > len(m.snapshots)-1 {
		target = len(m.snapshots) - 1
	}
	if target == m.curSnap {
		return m, nil
	}
	m.curSnap = target
	if m.curSnap == len(m.snapshots)-1 {
		// Landing on the head re-enables follow in live mode.
		m.follow = m.live
	} else {
		m.follow = false
	}
	cmds := []tea.Cmd{m.refreshRowsCmd()}
	if c := m.drillRefreshCmd(); c != nil {
		cmds = append(cmds, c)
	}
	return m, tea.Batch(cmds...)
}

// jumpToNearestIncident walks the (drill-aware) incident vector and
// snaps the timeline cursor to the closest entry whose severity isn't
// IncidentNone in `direction` (+1 forward, -1 backward). Flashes a
// status hint if there's nothing to jump to in that direction so the
// keypress doesn't feel like a no-op.
func (m Model) jumpToNearestIncident(direction int) (tea.Model, tea.Cmd) {
	incidents := m.effectiveIncidents()
	if len(incidents) == 0 || len(m.snapshots) == 0 || direction == 0 {
		return m, nil
	}
	lim := len(incidents)
	if lim > len(m.snapshots) {
		lim = len(m.snapshots)
	}
	target := -1
	if direction > 0 {
		for i := m.curSnap + 1; i < lim; i++ {
			if incidents[i] != model.IncidentNone {
				target = i
				break
			}
		}
	} else {
		for i := m.curSnap - 1; i >= 0; i-- {
			if i >= lim {
				continue
			}
			if incidents[i] != model.IncidentNone {
				target = i
				break
			}
		}
	}
	if target < 0 {
		dir := "after"
		if direction < 0 {
			dir = "before"
		}
		m.statusFlash = "no further incidents " + dir + " this snapshot"
		m.flashUntil = time.Now().Add(2 * time.Second)
		return m, nil
	}
	return m.jumpSnap(target - m.curSnap)
}

// pageStep is "about 1% of the timeline", min 25. Scales naturally:
// 100 snaps → 25/step, 1175 snaps → 25/step, 10000 snaps → 100/step.
func (m Model) pageStep() int {
	step := len(m.snapshots) / 100
	if step < 25 {
		step = 25
	}
	return step
}

// podCellStyles produces per-(row, column) style hints for the pods table
// so unhealthy pods light up: yellow for transient soft-bad states
// (Pending / Terminating / ContainerCreating / Init:*), red for concrete
// failures (CrashLoopBackOff / OOMKilled / etc.) and a red READY cell
// when a pod is Running with not-all-ready (probe failures).
//
// Returns nil for non-pod kinds so the table renderer's plain path runs
// unchanged.
func (m Model) podCellStyles() [][]lipgloss.Style {
	if m.curKind >= len(m.kinds) || m.kinds[m.curKind].Kind != "pods" {
		return nil
	}
	// Cell indices in the pods catalog (see model.defPods): NAMESPACE(0),
	// NAME(1), READY(2), STATUS(3), …
	const readyCol, statusCol = 2, 3
	out := make([][]lipgloss.Style, len(m.rows))
	for i, r := range m.rows {
		if r.Shrunk || len(r.Cells) <= statusCol {
			continue
		}
		styles := make([]lipgloss.Style, len(r.Cells))
		switch model.ClassifyStatusOnly(r.Cells[statusCol]) {
		case model.HealthHardBad:
			styles[statusCol] = StyleIncidentRed
		case model.HealthSoftBad:
			styles[statusCol] = StyleIncidentYellow
		}
		// READY is red iff the pod is Running but probes aren't all green.
		// Other states (Pending, ContainerCreating, …) naturally have
		// READY != n/n, but that's expected — STATUS already conveys it.
		if r.Cells[statusCol] == "Running" && !model.ReadyComplete(r.Cells[readyCol]) {
			styles[readyCol] = StyleIncidentRed
		}
		out[i] = styles
	}
	return out
}

// effectiveIncidents returns the severity vector that should drive the
// timeline markers. Without a drill, that's the global vector built from
// every captured pod. With a drill active, we union just the per-pod
// vectors of pods in the drill's set — so the markers reflect the same
// resources the user has narrowed the table to.
func (m Model) effectiveIncidents() []model.IncidentSeverity {
	if m.drill == nil || len(m.incidentsPerPod) == 0 || len(m.drill.podSet) == 0 {
		return m.incidents
	}
	n := len(m.snapshots)
	if n == 0 {
		return nil
	}
	out := make([]model.IncidentSeverity, n)
	for key := range m.drill.podSet {
		v, ok := m.incidentsPerPod[key]
		if !ok {
			continue
		}
		lim := len(v)
		if lim > n {
			lim = n
		}
		for i := 0; i < lim; i++ {
			if v[i] > out[i] {
				out[i] = v[i]
			}
		}
	}
	return out
}

// flashShrunk shows a status-bar hint when the user invokes an action that
// requires per-resource detail data, but the row is marked shrunk and that
// data was stripped by `kk shrink`.
func (m *Model) flashShrunk(action string) {
	m.statusFlash = fmt.Sprintf("%s unavailable: this row was stripped by `kk shrink`", action)
	m.flashUntil = time.Now().Add(3 * time.Second)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// --- small helpers ---

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Run is the public entry point used by main. runner may be nil for replay
// mode; it is required for the live-log-stream behavior in record mode.
func Run(ctx context.Context, st *store.Store, live bool, progress <-chan capture.Tick, runner *kubectl.Runner, cancel context.CancelFunc) error {
	m := NewModel(st, live, progress, runner, cancel)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

// silence "unused" warnings for lipgloss in case future styling changes.
var _ = lipgloss.NewStyle
