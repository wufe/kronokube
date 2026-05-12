// Command kk (KronoKube) records read-only snapshots of a Kubernetes cluster
// into a single seekable .kk file and replays them with a TUI.
//
// USAGE
//   kk record [--out file.kk] [--interval 30s] [--config kk.yaml] [--namespace ns ...]
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
	"strings"
	"syscall"
	"time"

	"github.com/wufe/kronokube/internal/capture"
	"github.com/wufe/kronokube/internal/config"
	"github.com/wufe/kronokube/internal/kubectl"
	"github.com/wufe/kronokube/internal/store"
	"github.com/wufe/kronokube/internal/tui"
)

const usage = `kk — KronoKube, a read-only Kubernetes time machine

Commands:
  record    Capture snapshots of a cluster into a .kk file (TUI live mode)
  replay    Open an existing .kk file and scrub through its history
  safety    Print the kubectl allowlist (audit aid)

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
	case "safety", "audit":
		runSafetyAudit()
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
	cfgPath := fs.String("config", "", "YAML config file (optional)")
	interval := fs.Duration("interval", 0, "snapshot interval (overrides config; default 30s)")
	contextName := fs.String("context", "", "kubeconfig context (overrides config; default current-context)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (overrides config / $KUBECONFIG)")
	includeNS := fs.String("namespace", "", "comma-separated namespaces to include (default: all)")
	excludeNS := fs.String("exclude-namespace", "", "comma-separated namespaces to skip")
	headless := fs.Bool("no-tui", false, "record without a TUI (useful for daemons / cron)")
	logsOn := fs.Bool("logs", false, "capture a tail of pod logs each snapshot (overrides config)")
	logsTail := fs.Int("logs-tail", 100, "per-container tail when --logs is set")
	logsTimeout := fs.Duration("logs-timeout", 5*time.Second, "per-pod timeout for log fetch when --logs is set")
	_ = fs.Parse(args)

	// Track which flags were explicitly provided so we only override the
	// config when the user actually said something.
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		die(err)
	}
	// CLI flags override config.
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
	if setFlags["logs"] {
		cfg.PodLogs.Enabled = *logsOn
	}
	if setFlags["logs-tail"] {
		cfg.PodLogs.TailLines = *logsTail
	}
	if setFlags["logs-timeout"] {
		cfg.PodLogs.PerPodTimeout = *logsTimeout
	}

	runner := kubectl.NewRunner("", cfg.Context, cfg.Kubeconfig)

	// Resolve actual context name (for default filename + meta).
	resolvedCtx, _ := runner.CurrentContext(context.Background())
	if cfg.Context == "" {
		cfg.Context = resolvedCtx
	}

	if *out == "" {
		*out = defaultOutFile(cfg.Context)
	}
	if cfg.Output != "" && *out == "" {
		*out = cfg.Output
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

	snap := capture.New(cfg, runner, st)
	progressCh := snap.Progress()

	// Start capture in background.
	captureErr := make(chan error, 1)
	go func() { captureErr <- snap.Run(ctx) }()

	fmt.Fprintf(os.Stderr, "kk: recording to %s (interval %s, context %q)\n", *out, cfg.Interval, cfg.Context)

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
	if err := tui.Run(ctx, st, true, progressCh, cancel); err != nil {
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
	if err := tui.Run(ctx, st, false, nil, cancel); err != nil {
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
	fmt.Fprintf(os.Stderr, "[%s] snap %d  ok=%d forbidden=%d skipped=%d error=%d\n",
		t.Timestamp.Format(time.RFC3339), t.SnapshotID, ok, fb, sk, er)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "kk: "+err.Error())
	os.Exit(1)
}
