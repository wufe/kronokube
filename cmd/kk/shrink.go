package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

// runShrink implements `kk shrink <file.kk> [-o output.kk] [-y]`.
//
// The rule (matches what was discussed before implementation):
//
//   - For every captured pod we compute the set of snapshot indices where
//     the pod was unhealthy (HealthSoftBad or HealthHardBad according to
//     model.ClassifyPodHealth).
//
//   - The pod's "important" indices are that set expanded by ±1 — so the
//     snapshot before and after each unhealthy moment are also preserved
//     (they give context for whatever went wrong).
//
//   - Every (pod, snapshot) pair that's NOT in the important set has its
//     resource blob replaced with the shared empty blob and its captured
//     log dropped. The cells_json row stays so the pod keeps showing up
//     in the tabular view, just greyed-out and with d/y/l/t disabled.
//
//   - Non-pod resources are untouched. Without a health signal we have no
//     basis for deciding what's interesting and what isn't.
func runShrink(args []string) {
	fs := flag.NewFlagSet("shrink", flag.ExitOnError)
	out := fs.String("o", "", "write the shrunk file here instead of modifying the input in place")
	yes := fs.Bool("y", false, "skip the confirmation prompt")
	positional := parseFlagsMixed(fs, args)

	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "kk shrink <file.kk> [-o output.kk] [-y]")
		os.Exit(2)
	}
	inPath := positional[0]
	if _, err := os.Stat(inPath); err != nil {
		die(err)
	}

	workPath := inPath
	if *out != "" {
		if err := copyFile(inPath, *out); err != nil {
			die(err)
		}
		workPath = *out
		fmt.Fprintf(os.Stderr, "kk shrink: copied %s -> %s\n", inPath, workPath)
	}

	st, err := store.Open(workPath)
	if err != nil {
		die(err)
	}
	defer st.Close()

	snapshots, err := st.ListSnapshots()
	if err != nil {
		die(err)
	}
	if len(snapshots) == 0 {
		fmt.Fprintln(os.Stderr, "kk shrink: no snapshots in this file")
		return
	}

	rows, err := st.IteratePodHealthRows()
	if err != nil {
		die(err)
	}

	targets := computeShrinkTargets(rows, snapshots)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "kk shrink: nothing to do — every captured pod is either always-unhealthy or in the ±1 incident window throughout")
		return
	}

	// Pre-shrink summary.
	uniquePods := map[string]struct{}{}
	for _, t := range targets {
		uniquePods[t.Namespace+"/"+t.Pod] = struct{}{}
	}
	fmt.Fprintf(os.Stderr, "kk shrink: %d (pod, snapshot) pairs to strip, covering %d distinct pods, in %s\n",
		len(targets), len(uniquePods), workPath)

	if !*yes {
		if !confirm("Proceed? [y/N] ") {
			fmt.Fprintln(os.Stderr, "kk shrink: aborted")
			os.Exit(1)
		}
	}

	stats, err := runShrinkWithUI(st, targets)
	if err != nil {
		die(err)
	}

	fmt.Fprintf(os.Stderr, "kk shrink: done. rows marked=%d, pod_logs deleted=%d, blobs %d -> %d, size %s -> %s\n",
		stats.RowsMarked, stats.PodLogsDeleted,
		stats.BlobsBefore, stats.BlobsAfter,
		humanBytes(stats.BytesBefore), humanBytes(stats.BytesAfter))
}

// runShrinkWithUI runs Store.Shrink under a tiny Bubble Tea program that
// shows a progress bar fed by Shrink's callback. The program returns once
// the work goroutine signals completion. Errors from Shrink are
// propagated back through the model's `err` field.
//
// When stderr isn't attached to a terminal (e.g. CI, piped to a file),
// the Bubble Tea program would error out trying to open /dev/tty; we fall
// back to plain phase-change printing in that case.
func runShrinkWithUI(st *store.Store, targets []store.ShrinkTarget) (store.ShrinkStats, error) {
	if !isTerminal(os.Stderr) {
		return shrinkPlain(st, targets)
	}
	m := newShrinkUI()
	p := tea.NewProgram(m)

	type result struct {
		stats store.ShrinkStats
		err   error
	}
	resCh := make(chan result, 1)

	go func() {
		stats, err := st.Shrink(targets, func(done, total int, phase string) {
			p.Send(shrinkProgressMsg{done: done, total: total, phase: phase})
		})
		// Make sure the bar ends at 100% so the final frame looks right
		// even if the caller didn't emit a last "stripping" tick.
		p.Send(shrinkProgressMsg{done: 1, total: 1, phase: "done"})
		p.Send(shrinkDoneMsg{stats: stats, err: err})
		resCh <- result{stats, err}
	}()

	finalM, err := p.Run()
	if err != nil {
		return store.ShrinkStats{}, err
	}
	// User pressed Ctrl+C: the goroutine may still be inside a long SQL
	// statement (typically VACUUM). Don't block on resCh — bail out with
	// the standard SIGINT exit code. SQLite is safe under abrupt process
	// termination: the WAL/rollback journal keeps the file consistent and
	// the uncommitted transaction is rolled back automatically on the
	// next open.
	if mm, ok := finalM.(shrinkUIModel); ok && mm.interrupted {
		fmt.Fprintln(os.Stderr, "kk shrink: interrupted; the file is unchanged")
		os.Exit(130)
	}
	r := <-resCh
	return r.stats, r.err
}

// shrinkPlain is the non-TTY fallback. It just prints one line per phase
// transition so users running shrink in CI still get a status trail.
func shrinkPlain(st *store.Store, targets []store.ShrinkTarget) (store.ShrinkStats, error) {
	lastPhase := ""
	return st.Shrink(targets, func(done, total int, phase string) {
		if phase == lastPhase {
			return
		}
		lastPhase = phase
		fmt.Fprintf(os.Stderr, "kk shrink: %s (%d/%d)\n", phase, done, total)
	})
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// --- shrink progress UI ---

type shrinkProgressMsg struct {
	done, total int
	phase       string
}

type shrinkDoneMsg struct {
	stats store.ShrinkStats
	err   error
}

type shrinkUIModel struct {
	bar         progress.Model
	done        int
	total       int
	phase       string
	finished    bool
	interrupted bool
	err         error
}

func newShrinkUI() shrinkUIModel {
	return shrinkUIModel{
		bar:   progress.New(progress.WithDefaultGradient(), progress.WithWidth(40)),
		phase: "starting",
	}
}

func (m shrinkUIModel) Init() tea.Cmd { return nil }

func (m shrinkUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case shrinkProgressMsg:
		// We render directly via ViewAs(pct) instead of animating with
		// SetPercent: the strip phase fires updates faster than the
		// spring animation can settle, leaving the bar visually parked
		// at 0% even though our counters were marching forward.
		m.done, m.total, m.phase = msg.done, msg.total, msg.phase
		return m, nil
	case shrinkDoneMsg:
		m.finished = true
		m.err = msg.err
		return m, tea.Quit
	case tea.KeyMsg:
		// Bubble Tea runs the terminal in raw mode, so the kernel won't
		// turn Ctrl+C into SIGINT — we have to handle it. On Ctrl+C we
		// mark the model interrupted and quit; the caller will exit the
		// process without waiting for the SQL goroutine. SQLite's WAL
		// makes that safe: the in-flight transaction simply doesn't
		// commit and the file remains consistent.
		if msg.Type == tea.KeyCtrlC {
			m.interrupted = true
			return m, tea.Quit
		}
		// Other keys are ignored — there's no useful single-key action
		// while a shrink is running.
	}
	return m, nil
}

func (m shrinkUIModel) View() string {
	if m.err != nil {
		return fmt.Sprintf("  shrink failed: %s\n", m.err.Error())
	}
	if m.total <= 0 {
		return fmt.Sprintf("  %s…\n", m.phase)
	}
	pct := float64(m.done) / float64(m.total)
	if pct > 1 {
		pct = 1
	}
	return fmt.Sprintf("  %s — %d/%d\n  %s\n", m.phase, m.done, m.total, m.bar.ViewAs(pct))
}

// computeShrinkTargets walks the pre-fetched rows (already ordered by
// namespace, name, snapshot_id) and emits one ShrinkTarget per (pod,
// snapshot) pair that falls outside the pod's ±1 window around its
// unhealthy snapshots.
func computeShrinkTargets(rows []store.PodHealthRow, snapshots []store.SnapshotInfo) []store.ShrinkTarget {
	if len(snapshots) == 0 {
		return nil
	}
	snapIdx := make(map[int64]int, len(snapshots))
	for i, s := range snapshots {
		snapIdx[s.ID] = i
	}

	type obs struct {
		idx    int
		snapID int64
		health model.PodHealth
	}

	var targets []store.ShrinkTarget
	var curObs []obs
	var curNs, curName string

	flush := func() {
		if len(curObs) == 0 {
			return
		}
		// Collect unhealthy indices and expand by ±1.
		important := make(map[int]struct{})
		for _, o := range curObs {
			if o.health == model.HealthHealthy {
				continue
			}
			important[o.idx] = struct{}{}
			if o.idx > 0 {
				important[o.idx-1] = struct{}{}
			}
			if o.idx < len(snapshots)-1 {
				important[o.idx+1] = struct{}{}
			}
		}
		for _, o := range curObs {
			if _, keep := important[o.idx]; keep {
				continue
			}
			targets = append(targets, store.ShrinkTarget{
				SnapshotID: o.snapID,
				Namespace:  curNs,
				Pod:        curName,
			})
		}
	}

	for _, r := range rows {
		if r.Namespace != curNs || r.Name != curName {
			flush()
			curNs, curName = r.Namespace, r.Name
			curObs = curObs[:0]
		}
		i, ok := snapIdx[r.SnapshotID]
		if !ok {
			continue
		}
		curObs = append(curObs, obs{
			idx:    i,
			snapID: r.SnapshotID,
			health: model.ClassifyPodHealth(r.Status, r.Ready),
		})
	}
	flush()

	return targets
}

// parseFlagsMixed parses args allowing flags before, between, or after
// positional arguments — unlike flag.Parse, which stops at the first
// non-flag and silently leaves later flags untouched. Returns the
// accumulated positional arguments in order.
func parseFlagsMixed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	remaining := args
	for len(remaining) > 0 {
		if err := fs.Parse(remaining); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		remaining = fs.Args()[1:]
	}
	return positional
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
