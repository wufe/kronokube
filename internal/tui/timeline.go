package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/wufe/kronokube/internal/store"
)

// renderTimelineBar draws the snapshot scrubber: a horizontal line with
// markers, a cursor for the current position, and label timestamps. width is
// the available pixel width; snapshots is the full list; cur is the index
// of the currently-displayed snapshot. following controls the cursor glyph:
// a solid ● when the view auto-advances with new snapshots, a hollow ◇ when
// the user has paused on a specific point.
func renderTimelineBar(width int, snapshots []store.SnapshotInfo, cur int, following bool) string {
	if len(snapshots) == 0 {
		return StyleMuted.Render("(no snapshots yet)")
	}
	// Reserve right side for "snap N/M" counter.
	counter := fmt.Sprintf(" snap %d/%d ", cur+1, len(snapshots))
	barWidth := width - len(counter) - 2
	if barWidth < 10 {
		barWidth = 10
	}

	bar := make([]rune, barWidth)
	for i := range bar {
		bar[i] = '─'
	}
	// Position the cursor.
	pos := 0
	if len(snapshots) > 1 {
		pos = (cur * (barWidth - 1)) / (len(snapshots) - 1)
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= barWidth {
		pos = barWidth - 1
	}

	// Place a few tick markers spaced along the bar; ◆ at first/last/mid.
	for _, idx := range tickPositions(barWidth, 5) {
		bar[idx] = '┼'
	}
	cursor := '●'
	if !following {
		cursor = '◇'
	}
	bar[pos] = cursor

	// Build the label row underneath (sparse — start, cursor, end).
	labels := make([]rune, barWidth)
	for i := range labels {
		labels[i] = ' '
	}
	curTS := snapshots[cur].Timestamp.Format("15:04:05")
	placeLabel(labels, 0, snapshots[0].Timestamp.Format("15:04:05"))
	placeLabelCenteredAt(labels, pos, curTS)
	placeLabelRight(labels, barWidth-1, snapshots[len(snapshots)-1].Timestamp.Format("15:04:05"))

	return StyleTimeline.Render(string(bar)) + StyleStatusBar.Render(counter) + "\n" + StyleMuted.Render(string(labels))
}

func tickPositions(width, n int) []int {
	if n < 2 || width < n {
		return nil
	}
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = (i * (width - 1)) / (n - 1)
	}
	return out
}

func placeLabel(buf []rune, at int, label string) {
	for i, r := range label {
		idx := at + i
		if idx < 0 || idx >= len(buf) {
			return
		}
		buf[idx] = r
	}
}

func placeLabelCenteredAt(buf []rune, pos int, label string) {
	start := pos - len(label)/2
	if start < 0 {
		start = 0
	}
	if start+len(label) > len(buf) {
		start = len(buf) - len(label)
		if start < 0 {
			start = 0
		}
	}
	for i, r := range label {
		idx := start + i
		if idx < 0 || idx >= len(buf) {
			return
		}
		buf[idx] = r
	}
}

func placeLabelRight(buf []rune, end int, label string) {
	start := end - len(label) + 1
	placeLabel(buf, start, label)
}

// formatSnapAgo returns "5s ago", "12m ago", "2h ago" for status bar use.
func formatSnapAgo(ts time.Time) string {
	d := time.Since(ts)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// joinKindTabs renders the kind switcher row at the top.
func joinKindTabs(names []string, current int) string {
	var b strings.Builder
	for i, n := range names {
		if i == current {
			b.WriteString(StyleSelected.Render(" " + n + " "))
		} else {
			b.WriteString(" " + StyleMuted.Render(n) + " ")
		}
	}
	return b.String()
}
