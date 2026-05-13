package tui

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wufe/kronokube/internal/kubectl"
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

// spawnLoglensLive launches loglens against a continuously-streaming pod log.
// The kk TUI's own kubectl stream is expected to be stopped by the caller
// before this runs; we start a fresh `kubectl logs -f` and feed it (plus
// the snapshot the user was already seeing) into loglens stdin. Loglens
// thus shows the same history followed by ongoing lines as they arrive.
//
// Returns:
//   - cmd, nil    : a tea.Cmd that suspends kk until loglens exits
//   - nil, nil    : loglens is not on PATH
//   - nil, err    : kubectl invocation failed validation or exec
//
// kubectl is reaped when loglens exits, regardless of how it exited.
func spawnLoglensLive(runner *kubectl.Runner, ns, pod string, snapshot []byte) (tea.Cmd, error) {
	bin := loglensPath()
	if bin == "" {
		return nil, nil
	}
	if runner == nil {
		// Defensive: callers gate on m.live before invoking us.
		return nil, errLogStreamNoRunner
	}
	// Fresh streaming kubectl, separate from the TUI's own. Loglens drives
	// its lifetime: we kill kubectl in the ExecProcess callback below.
	kpipe, _, kstop, err := runner.LogsStream(context.Background(), ns, pod, liveLogMaxLines)
	if err != nil {
		return nil, err
	}

	// Loglens stdin gets the snapshot first, then ongoing kubectl output.
	// io.Pipe gives us a synchronous handoff: loglens reads as fast as it
	// can, kubectl blocks if it doesn't (back-pressure all the way to the
	// kernel pipe between us and kubectl). We do *not* try to bound this —
	// loglens manages its own memory.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		if len(snapshot) > 0 {
			if _, werr := pw.Write(snapshot); werr != nil {
				return
			}
		}
		_, _ = io.Copy(pw, kpipe)
	}()

	cmd := exec.Command(bin)
	cmd.Stdin = pr
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		// Tear down kubectl as soon as loglens is done. The copier goroutine
		// will see EOF on the kubectl side and exit shortly after.
		kstop()
		return loglensExitedMsg{err: err}
	}), nil
}
