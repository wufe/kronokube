package tui

import (
	"bytes"
	"context"
	"io"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wufe/kronokube/internal/kubectl"
)

// liveLogStream is a running `kubectl logs -f` against one pod, with a
// goroutine reading its stdout into a bounded ring buffer. It is created
// when the user opens the logs view in live+follow mode and torn down when
// they leave that view (Esc / view switch / quit). Loglens, when launched
// from the streaming view, spawns its own short-lived stream via
// logs_spawn.go — this struct keeps the kk-side display alive.
type liveLogStream struct {
	ns, pod string

	stop func() // cancels the kubectl process and reaps it
	ch   chan logChunkMsg
	buf  *logRing
}

// liveLogMaxLines is the cap on lines kept in the in-memory ring buffer.
// Once exceeded, the oldest lines are dropped (FIFO). Loglens, when invoked,
// gets its own independent stream and manages its own memory.
const liveLogMaxLines = 3000

// logChunkMsg delivers one read from kubectl's stdout to the bubbletea
// Update loop. target identifies the pod ("ns/pod") so messages that race
// past a navigation can be ignored. When done is true the stream has ended;
// err carries the reason (kubectl exit error, stderr text, or nil for EOF).
type logChunkMsg struct {
	target string
	chunk  []byte
	done   bool
	err    error
}

// startLiveLogStream forks `kubectl logs -f`, wires its stdout to a goroutine
// that fills a ring buffer, and returns the handle. The Update loop pulls
// chunks off with waitForLogChunkCmd. Returns an error only for synchronous
// failures (validation, exec); kubectl-runtime failures arrive via the
// channel as a final logChunkMsg{done:true, err:…}.
func startLiveLogStream(runner *kubectl.Runner, ns, pod string) (*liveLogStream, error) {
	if runner == nil {
		return nil, errLogStreamNoRunner
	}
	pipe, stderr, stop, err := runner.LogsStream(context.Background(), ns, pod, liveLogMaxLines)
	if err != nil {
		return nil, err
	}
	s := &liveLogStream{
		ns:   ns,
		pod:  pod,
		stop: stop,
		ch:   make(chan logChunkMsg, 16),
		buf:  newLogRing(liveLogMaxLines),
	}
	target := ns + "/" + pod
	go func() {
		defer close(s.ch)
		readBuf := make([]byte, 4096)
		for {
			n, rerr := pipe.Read(readBuf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, readBuf[:n])
				s.buf.write(chunk)
				select {
				case s.ch <- logChunkMsg{target: target, chunk: chunk}:
				default:
					// Updater is behind; the next chunk merges anyway so a
					// dropped notify isn't a correctness issue — the next
					// successful send carries up-to-date buffer state.
				}
			}
			if rerr != nil {
				finalErr := rerr
				if rerr == io.EOF {
					finalErr = nil
				}
				// Surface kubectl's stderr instead of a bare "exit status 1"
				// when the stream died with output (RBAC, pod not found).
				if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
					finalErr = &logStreamErr{stderr: string(msg), wrapped: rerr}
				}
				s.ch <- logChunkMsg{target: target, done: true, err: finalErr}
				return
			}
		}
	}()
	return s, nil
}

// Snapshot returns the current ring-buffer contents. Safe to call from any
// goroutine. The slice is independent of the buffer's internal storage.
func (s *liveLogStream) Snapshot() []byte {
	if s == nil {
		return nil
	}
	return s.buf.snapshot()
}

// Stop cancels the underlying kubectl process and waits for its goroutine
// to drain. Safe to call more than once.
func (s *liveLogStream) Stop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stop()
	s.stop = nil
}

// Target returns "namespace/pod" for matching against late chunks.
func (s *liveLogStream) Target() string {
	if s == nil {
		return ""
	}
	return s.ns + "/" + s.pod
}

// waitForLogChunkCmd reads exactly one chunk off the streamer's channel and
// returns it as a bubbletea message. Update re-arms it after each message,
// the same pattern waitForTickCmd uses for capture progress.
func waitForLogChunkCmd(ch <-chan logChunkMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// logRing is a thread-safe ring buffer of lines, with a tail of bytes that
// haven't yet seen a newline. Bytes are flushed into lines at every \n;
// once the line count exceeds capLines we trim from the front.
type logRing struct {
	mu       sync.Mutex
	lines    [][]byte
	partial  []byte
	capLines int
}

func newLogRing(capLines int) *logRing {
	if capLines < 1 {
		capLines = 1
	}
	return &logRing{capLines: capLines}
}

func (r *logRing) write(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := append(r.partial, chunk...)
	r.partial = nil
	for {
		nl := bytes.IndexByte(pending, '\n')
		if nl < 0 {
			r.partial = append([]byte(nil), pending...)
			break
		}
		line := make([]byte, nl)
		copy(line, pending[:nl])
		r.lines = append(r.lines, line)
		pending = pending[nl+1:]
	}
	if len(r.lines) > r.capLines {
		excess := len(r.lines) - r.capLines
		// Shift down so we don't hold references to old line slices.
		copy(r.lines, r.lines[excess:])
		for i := r.capLines; i < len(r.lines); i++ {
			r.lines[i] = nil
		}
		r.lines = r.lines[:r.capLines]
	}
}

func (r *logRing) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out bytes.Buffer
	for _, ln := range r.lines {
		out.Write(ln)
		out.WriteByte('\n')
	}
	out.Write(r.partial)
	return out.Bytes()
}

type logStreamErr struct {
	stderr  string
	wrapped error
}

func (e *logStreamErr) Error() string {
	if e.wrapped != nil {
		return e.stderr + " (" + e.wrapped.Error() + ")"
	}
	return e.stderr
}
func (e *logStreamErr) Unwrap() error { return e.wrapped }

// errLogStreamNoRunner is returned when live streaming is requested in a
// context where no kubectl runner is available (i.e. replay mode). The TUI
// only triggers streaming in live mode, so this is a programming-error
// guard rather than a user-facing condition.
var errLogStreamNoRunner = &logStreamErr{stderr: "live log stream requested but no kubectl runner is wired"}
