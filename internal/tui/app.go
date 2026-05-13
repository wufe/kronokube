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
// cancel may be nil; otherwise it is called on Quit.
func NewModel(st *store.Store, live bool, progress <-chan capture.Tick, cancel context.CancelFunc) Model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter (regex-free substring)"
	ti.CharLimit = 200

	m := Model{
		store:       st,
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
}

type namespacesLoadedMsg struct {
	namespaces []string
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

func loadEventsCmd(s *store.Store, uid string, snapID int64, kind model.Kind, ns, name string) tea.Cmd {
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
		return detailLoadedMsg{view: viewEvents, events: evs, content: renderEventsList(kind, ns, name, evs)}
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
		return detailLoadedMsg{view: viewLogs, content: renderPodLogs(ns, pod, pl)}
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
		return m, nil

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
	f := strings.ToLower(strings.TrimSpace(m.filterInput.Value()))
	if f == "" {
		m.rows = m.allRows
		return
	}
	m.rows = m.rows[:0]
	for _, r := range m.allRows {
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
		switch {
		case key.Matches(msg, k.Quit):
			return m.quit()
		case key.Matches(msg, k.Back):
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
		if m.curKind > 0 {
			m.curKind--
		} else {
			m.curKind = len(m.kinds) - 1
		}
		m.selRow, m.scroll = 0, 0
		return m, m.refreshRowsCmd()
	case key.Matches(msg, k.NextKind):
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
	case key.Matches(msg, k.Describe), msg.Type == tea.KeyEnter:
		if r := m.currentRow(); r != nil {
			m.captureSelection(*r)
			return m, loadDescribeCmd(m.store, m.snapshots[m.curSnap].ID, r.Kind, r.Namespace, r.Name, r.UID)
		}
	case key.Matches(msg, k.Events):
		if r := m.currentRow(); r != nil {
			m.captureSelection(*r)
			return m, loadEventsCmd(m.store, r.UID, m.snapshots[m.curSnap].ID, r.Kind, r.Namespace, r.Name)
		}
	case key.Matches(msg, k.Timeline):
		if r := m.currentRow(); r != nil {
			m.captureSelection(*r)
			return m, loadChangeTimelineCmd(m.store, r.Kind, r.Namespace, r.Name)
		}
	case key.Matches(msg, k.Logs):
		if r := m.currentRow(); r != nil && r.Kind == "pods" {
			m.captureSelection(*r)
			return m, loadLogsCmd(m.store, m.snapshots[m.curSnap].ID, r.Namespace, r.Name)
		}
		// Non-pod selection: flash a hint rather than silently doing nothing.
		m.statusFlash = "logs view: select a pod"
		m.flashUntil = time.Now().Add(2 * time.Second)
	case key.Matches(msg, k.YAML):
		if r := m.currentRow(); r != nil {
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
	for i, r := range m.rows {
		rowCells[i] = r.Cells
	}
	visible := m.tableVisibleRows()
	b.WriteString(renderTable(headers, rowCells, m.width, m.selRow, m.scroll, visible))
	// Per-kind status note
	if st, ok := m.kindStatus[m.kinds[m.curKind].Kind]; ok && st != model.StatusOK {
		msg := string(st)
		if m.kindErrs[m.kinds[m.curKind].Kind] != "" {
			msg += " — " + m.kindErrs[m.kinds[m.curKind].Kind]
		}
		b.WriteString(StyleWarn.Render(msg))
		b.WriteString("\n")
	}
	// Timeline
	b.WriteString(renderTimelineBar(m.width, m.snapshots, m.curSnap, m.follow || !m.live))
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
	help := "tab: kind  /: filter  enter: describe  y: yaml  e: events  t: changes  o: logs  n: ns  ←/→: 1  ⇧←/⇧→: 10  </>: 1%  L: live  ?: help  q: quit"
	if time.Now().Before(m.flashUntil) && m.statusFlash != "" {
		return StyleOK.Render(m.statusFlash) + "  " + StyleMuted.Render(help)
	}
	return StyleStatusBar.Render(help)
}

func (m Model) tableVisibleRows() int {
	// Header(1) + tabs(1) + filter(1) + table-header(1) + warn(0/1) + tl-bar(2) + status(1).
	reserved := 8
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
	lines := strings.Split(body, "\n")
	start := m.detailScroll
	if start >= len(lines) {
		start = max0(len(lines) - 1)
	}
	visible := m.height - 4
	if visible < 5 {
		visible = 5
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
		b.WriteString(StyleMuted.Render("enter: jump to this snapshot   esc: back   q: quit"))
		return b.String()
	}

	body = strings.Join(lines[start:end], "\n")
	footer := StyleMuted.Render(fmt.Sprintf("line %d/%d   ↑↓ scroll   esc: back   q: quit", start+1, len(lines)))
	return header + "\n\n" + body + "\n" + footer
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
  ctrl-a / ctrl-e   first / last snapshot
  L                 jump to live (resume follow)

INSPECT
  enter / d         describe selected resource
  y                 raw YAML of selected resource (from captured data)
  e                 events for selected resource (across all snapshots)
  t                 changes timeline for selected resource
  o                 pod logs at this snapshot (when pod_logs.enabled in config)

OTHER
  ?                 help
  q / ctrl-c        quit
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
func renderEventsList(kind model.Kind, ns, name string, events []model.Event) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("Events for %s %s/%s — %d total", kind, ns, name, len(events))))
	b.WriteString("\n\n")
	if len(events) == 0 {
		b.WriteString(StyleMuted.Render("<none captured>"))
		return b.String()
	}
	// Sort newest first.
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].LastTimestamp.After(events[j].LastTimestamp)
	})
	for _, e := range events {
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
	return b.String()
}

// renderPodLogs formats the captured log tail for a pod. Nil means no record
// exists for this snapshot (either capture is disabled, or the pod wasn't
// present at this tick).
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
	return m, m.refreshRowsCmd()
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

// --- small helpers ---

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Run is the public entry point used by main.
func Run(ctx context.Context, st *store.Store, live bool, progress <-chan capture.Tick, cancel context.CancelFunc) error {
	m := NewModel(st, live, progress, cancel)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

// silence "unused" warnings for lipgloss in case future styling changes.
var _ = lipgloss.NewStyle
