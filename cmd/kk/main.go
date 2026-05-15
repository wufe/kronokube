// Command kk (KronoKube) records read-only snapshots of a Kubernetes cluster
// into a single seekable .kk file and replays them with a TUI.
//
// USAGE
//   kk record [--out file.kk] [--interval 30s] [--namespace ns ...]
//   kk replay <file.kk>
//   kk safety        — print the kubectl allowlist for audit
//
// SAFETY
//   The program shells out to kubectl only via internal/kubectl, which
//   refuses any verb or subcommand not on its read-only allowlist.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/wufe/kronokube/internal/capture"
	"github.com/wufe/kronokube/internal/config"
	"github.com/wufe/kronokube/internal/kubectl"
	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
	"github.com/wufe/kronokube/internal/tui"
)

// Overridable at build time:
//
//	go build -ldflags "-X main.version=v0.2.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/kk
//
// When unset (e.g. `go install` or `go run`), runVersion falls back to
// runtime/debug.ReadBuildInfo so the binary still prints something useful.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

const usage = `kk — KronoKube, a read-only Kubernetes time machine

Commands:
  record    Capture snapshots of a cluster into a .kk file (TUI live mode)
            Add --incidents-only to keep only snapshots in the ±1 window
            around a pod incident.
  replay    Open an existing .kk file and scrub through its history
  shrink    Strip non-essential data (logs / describe / yaml) from healthy pods
  safety    Print the kubectl allowlist (audit aid)
  version   Print the kk version and build info

Run "kk <command> -h" for command-specific flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "record":
		runRecord(os.Args[2:])
	case "replay":
		runReplay(os.Args[2:])
	case "shrink":
		runShrink(os.Args[2:])
	case "safety", "audit":
		runSafetyAudit()
	case "version", "--version", "-v":
		runVersion()
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintln(os.Stderr, usage)
		fmt.Fprintf(os.Stderr, "\nunknown command: %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runRecord(args []string) {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	out := fs.String("out", "", "output .kk file (default: kk-<context>-<date>.kk)")
	interval := fs.Duration("interval", 0, "snapshot interval (default 30s)")
	contextName := fs.String("context", "", "kubeconfig context (default: current-context)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	includeNS := fs.String("namespace", "", "comma-separated namespaces to include (default: all)")
	fs.StringVar(includeNS, "n", "", "shorthand for --namespace")
	excludeNS := fs.String("exclude-namespace", "", "comma-separated namespaces to skip")
	kindsSpec := fs.String("kinds", "", "preset (minimal|default|workloads|full) or comma-separated kinds (default: default)")
	excludeKinds := fs.String("exclude-kinds", "", "comma-separated kinds to drop from the resolved set")
	selector := fs.String("selector", "", "label selector passed as -l to every kubectl get")
	fs.StringVar(selector, "l", "", "shorthand for --selector")
	headless := fs.Bool("no-tui", false, "record without a TUI (useful for daemons / cron)")
	logsOn := fs.Bool("logs", false, "capture a tail of pod logs each snapshot")
	logsTail := fs.Int("logs-tail", 100, "per-container tail when --logs is set")
	logsTimeout := fs.Duration("logs-timeout", 5*time.Second, "per-pod timeout for log fetch when --logs is set")
	incidentsOnly := fs.Bool("incidents-only", false, "only persist snapshots in the ±1 window around a pod incident (same retention as `kk shrink`)")
	_ = fs.Parse(args)

	// Track which flags were explicitly provided so we only override the
	// config when the user actually said something.
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	cfg := config.Default()
	if *interval > 0 {
		cfg.Interval = *interval
	}
	if *contextName != "" {
		cfg.Context = *contextName
	}
	if *kubeconfig != "" {
		cfg.Kubeconfig = *kubeconfig
	}
	if *includeNS != "" {
		cfg.IncludeNamespaces = splitCSV(*includeNS)
	}
	if *excludeNS != "" {
		cfg.ExcludeNamespaces = splitCSV(*excludeNS)
	}
	if setFlags["kinds"] {
		cfg.Kinds = *kindsSpec
	}
	if *excludeKinds != "" {
		cfg.ExcludeKinds = splitCSV(*excludeKinds)
	}
	if *selector != "" {
		cfg.Selector = *selector
	}

	// Resolve kinds eagerly so unknown names fail at flag-parse time, not
	// halfway through a snapshot pass.
	resolvedKinds, err := model.ResolveKinds(cfg.Kinds, cfg.ExcludeKinds)
	if err != nil {
		die(err)
	}
	if setFlags["logs"] {
		cfg.PodLogs.Enabled = *logsOn
	}
	if setFlags["logs-tail"] {
		cfg.PodLogs.TailLines = *logsTail
	}
	if setFlags["logs-timeout"] {
		cfg.PodLogs.PerPodTimeout = *logsTimeout
	}
	if *incidentsOnly {
		cfg.Mode = config.ModeIncidentsOnly
	}

	runner := kubectl.NewRunner("", cfg.Context, cfg.Kubeconfig)
	runner.SetListSelector(cfg.Selector)

	// Resolve actual context name (for default filename + meta).
	resolvedCtx, _ := runner.CurrentContext(context.Background())
	if cfg.Context == "" {
		cfg.Context = resolvedCtx
	}

	if *out == "" {
		*out = defaultOutFile(cfg.Context)
	}

	st, err := store.Open(*out)
	if err != nil {
		die(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap signals.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	snap := capture.New(cfg, runner, st, resolvedKinds)
	progressCh := snap.Progress()

	// Start capture in background.
	captureErr := make(chan error, 1)
	go func() { captureErr <- snap.Run(ctx) }()

	selectorInfo := ""
	if cfg.Selector != "" {
		selectorInfo = fmt.Sprintf(", selector %q", cfg.Selector)
	}
	fmt.Fprintf(os.Stderr, "kk: recording to %s (interval %s, context %q, mode %s, %d kinds%s)\n",
		*out, cfg.Interval, cfg.Context, cfg.Mode, len(resolvedKinds), selectorInfo)

	if *headless {
		// Print one progress line per tick. The recorder owns its own lifecycle.
		for {
			select {
			case <-ctx.Done():
				err := <-captureErr
				if err != nil && err != context.Canceled {
					die(err)
				}
				return
			case t, ok := <-progressCh:
				if !ok {
					return
				}
				printTick(t)
			}
		}
	}

	// TUI takes over stdout.
	if err := tui.Run(ctx, st, true, progressCh, runner, cancel); err != nil {
		die(err)
	}
	cancel()
	<-captureErr
}

func runReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kk replay <file.kk>")
		os.Exit(2)
	}
	path := fs.Arg(0)
	if _, err := os.Stat(path); err != nil {
		die(err)
	}
	st, err := store.Open(path)
	if err != nil {
		die(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tui.Run(ctx, st, false, nil, nil, cancel); err != nil {
		die(err)
	}
}

// runSafetyAudit prints exactly which kubectl invocations the program may
// ever make. This is the canonical way to convince yourself the tool is
// read-only: there is one allowlist file, and this command lays it bare.
func runSafetyAudit() {
	fmt.Println("KronoKube safety audit")
	fmt.Println("======================")
	fmt.Println()
	fmt.Println("Only kubectl invocations that pass kubectl.Validate may run.")
	fmt.Println("To verify this claim, read:")
	fmt.Println("  internal/kubectl/commands.go  — the allowlist")
	fmt.Println("  internal/kubectl/runner.go    — the single exec choke-point")
	fmt.Println()
	fmt.Println("Quick probe of representative argv shapes:")
	cases := [][]string{
		{"get", "pods", "-A", "-o=json"},
		{"describe", "deployment", "foo", "-n", "ns"},
		{"config", "current-context"},
		{"auth", "can-i", "list", "pods", "--all-namespaces"},
		{"logs", "p1", "-n", "default", "--all-containers=true", "--prefix=true", "--tail=100"},
		// expected to be rejected:
		{"apply", "-f", "x.yaml"},
		{"delete", "pod", "foo"},
		{"exec", "-it", "pod", "--", "sh"},
		{"port-forward", "pod/foo", "8080:80"},
		{"scale", "deploy/foo", "--replicas=0"},
		{"config", "use-context", "x"},
		{"auth", "reconcile"},
		{"logs", "-f", "p1"},
		{"logs", "p1", "--follow"},
	}
	for _, c := range cases {
		err := kubectl.Validate(c)
		status := "OK"
		if err != nil {
			status = "BLOCKED: " + err.Error()
		}
		fmt.Printf("  kubectl %-40s  %s\n", strings.Join(c, " "), status)
	}
	fmt.Println()
	fmt.Println("Streaming carve-out (kubectl.ValidateStreamingLogs):")
	fmt.Println("  Only the live-tail entry point (Runner.LogsStream) may use")
	fmt.Println("  -f / --follow. All write-shaped tokens stay blocked.")
	streamCases := [][]string{
		{"logs", "p1", "-n", "default", "--all-containers=true", "--prefix=true", "--tail=3000", "-f"},
		{"logs", "p1", "--follow"},
		// must still be rejected even under the streaming variant:
		{"apply", "-f", "x.yaml"},
		{"delete", "pod", "foo"},
		{"exec", "-it", "pod", "--", "sh"},
	}
	for _, c := range streamCases {
		err := kubectl.ValidateStreamingLogs(c)
		status := "OK"
		if err != nil {
			status = "BLOCKED: " + err.Error()
		}
		fmt.Printf("  kubectl %-40s  %s\n", strings.Join(c, " "), status)
	}
}

func runVersion() {
	v, c, d := version, commit, date
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	fmt.Printf("kk %s\n", v)
	if c != "" {
		dirty := ""
		if modified {
			dirty = " (dirty)"
		}
		fmt.Printf("  commit: %s%s\n", c, dirty)
	}
	if d != "" {
		fmt.Printf("  built:  %s\n", d)
	}
	fmt.Printf("  go:     %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func defaultOutFile(ctx string) string {
	safe := strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(ctx)
	if safe == "" {
		safe = "cluster"
	}
	return filepath.Join(".", fmt.Sprintf("kk-%s-%s.kk", safe, time.Now().Format("20060102-150405")))
}

func printTick(t capture.Tick) {
	ok, fb, sk, er := 0, 0, 0, 0
	for _, s := range t.Stats {
		switch s.Status {
		case "ok":
			ok++
		case "forbidden":
			fb++
		case "skipped":
			sk++
		case "error":
			er++
		}
	}
	disposition := fmt.Sprintf("snap %d", t.SnapshotID)
	switch {
	case t.HasIncident:
		disposition += " (incident)"
	case t.PodsShrunk > 0:
		disposition += fmt.Sprintf(" (shrunk %d pods)", t.PodsShrunk)
	}
	fmt.Fprintf(os.Stderr, "[%s] %s  ok=%d forbidden=%d skipped=%d error=%d\n",
		t.Timestamp.Format(time.RFC3339), disposition, ok, fb, sk, er)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "kk: "+err.Error())
	os.Exit(1)
}
