package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderTable lays out rows as a fixed-width table that fits in width columns.
// It mirrors k9s's approach: greedy column widths based on max content,
// trimmed to fit available width with elision on the rightmost columns.
//
// selectedIdx is the visible row (after scroll) to highlight; -1 = none.
// scroll is the index of the first row to render.
// visibleRows is how many rows fit on screen.
func renderTable(headers []string, rows [][]string, width int, selectedIdx, scroll, visibleRows int) string {
	colWidths := computeColWidths(headers, rows, width)

	var b strings.Builder
	// Header
	b.WriteString(StyleHeader.Render(joinCells(headers, colWidths)))
	b.WriteString("\n")

	end := scroll + visibleRows
	if end > len(rows) {
		end = len(rows)
	}
	for i := scroll; i < end; i++ {
		line := joinCells(rows[i], colWidths)
		if i-scroll == selectedIdx {
			b.WriteString(StyleSelected.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	// Pad to fixed height so the layout doesn't jump as snapshots change.
	for i := end - scroll; i < visibleRows; i++ {
		b.WriteString("\n")
	}
	return b.String()
}

// computeColWidths picks widths summing to ~width.
// Algorithm: target = max(len(header), max(len(cell) for all rows in column)) + 1.
// Then if the sum overflows width, shrink the widest column iteratively.
func computeColWidths(headers []string, rows [][]string, width int) []int {
	if len(headers) == 0 {
		return nil
	}
	w := make([]int, len(headers))
	for i, h := range headers {
		w[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i >= len(w) {
				break
			}
			if cw := lipgloss.Width(c); cw > w[i] {
				w[i] = cw
			}
		}
	}
	// +1 column padding
	for i := range w {
		w[i]++
	}
	sum := 0
	for _, x := range w {
		sum += x
	}
	for sum > width && width > 0 {
		// Shrink the widest column by 1.
		maxIdx := 0
		for i := 1; i < len(w); i++ {
			if w[i] > w[maxIdx] {
				maxIdx = i
			}
		}
		if w[maxIdx] <= 4 {
			break
		}
		w[maxIdx]--
		sum--
	}
	return w
}

func joinCells(cells []string, widths []int) string {
	var b strings.Builder
	for i, w := range widths {
		var c string
		if i < len(cells) {
			c = cells[i]
		}
		if lipgloss.Width(c) > w-1 && w > 1 {
			c = truncRunes(c, w-1)
		}
		b.WriteString(c)
		// pad
		pad := w - lipgloss.Width(c)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
	return b.String()
}

func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return "…"
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
