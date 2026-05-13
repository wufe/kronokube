package tui

import (
	"bytes"
	"os/exec"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// loglens integration is fully optional. KronoKube does not link against
// loglens; it just calls the binary if it happens to be on PATH. When
// pressed, "open in loglens" suspends the kk TUI, runs `loglens` in the
// foreground with the captured pod-log bytes piped to its stdin, and
// resumes when the user quits loglens.
//
// We probe for the binary once and cache the result; the user can install
// loglens after starting kk, but we won't notice until restart.

var (
	loglensOnce sync.Once
	loglensBin  string
)

// loglensPath returns the absolute path of the loglens binary if found on
// PATH, otherwise "". Safe to call repeatedly.
func loglensPath() string {
	loglensOnce.Do(func() {
		if p, err := exec.LookPath("loglens"); err == nil {
			loglensBin = p
		}
	})
	return loglensBin
}

// loglensExitedMsg is dispatched after the loglens subprocess returns.
// err is non-nil only for spawn / exec-level failures; a normal user quit
// (loglens exits 0) arrives with err==nil.
type loglensExitedMsg struct{ err error }

// spawnLoglens returns a tea.Cmd that hands `content` to loglens via stdin
// and suspends the program until loglens exits. Returns nil if loglens
// isn't available or there's nothing to view — the caller should handle
// that case (a status flash) before calling.
//
// --no-follow keeps loglens parked at the top of the captured tail instead
// of auto-scrolling to the tail-of-tail. We intentionally do NOT pass
// --exit-on-eof: the user wants to browse, not see loglens exit immediately
// when our finite buffer drains.
func spawnLoglens(content []byte) tea.Cmd {
	bin := loglensPath()
	if bin == "" || len(content) == 0 {
		return nil
	}
	cmd := exec.Command(bin, "--no-follow")
	cmd.Stdin = bytes.NewReader(content)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return loglensExitedMsg{err: err}
	})
}
