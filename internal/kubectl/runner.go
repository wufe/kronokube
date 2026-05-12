package kubectl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrForbidden is returned when an argv would do something other than read.
var ErrForbidden = errors.New("kubectl invocation rejected by allowlist")

// ErrForbiddenCategory and friends are wrapped by ErrForbidden so callers
// can distinguish reasons for tests/logging.
type forbiddenReason struct{ reason string }

func (e *forbiddenReason) Error() string  { return e.reason }
func (e *forbiddenReason) Unwrap() error  { return ErrForbidden }
func (e *forbiddenReason) Is(t error) bool { return t == ErrForbidden }

// Runner executes kubectl. Construct with NewRunner; never invoke exec.Command
// for kubectl from anywhere else in the codebase.
type Runner struct {
	binary string
	// extraArgs is prepended to every invocation (e.g. --context, --kubeconfig).
	extraArgs []string
}

// NewRunner builds a Runner. binary may be "" to use "kubectl" from PATH.
// contextName and kubeconfig may be "" to use defaults.
func NewRunner(binary, contextName, kubeconfig string) *Runner {
	if binary == "" {
		binary = "kubectl"
	}
	var extra []string
	if contextName != "" {
		extra = append(extra, "--context", contextName)
	}
	if kubeconfig != "" {
		extra = append(extra, "--kubeconfig", kubeconfig)
	}
	return &Runner{binary: binary, extraArgs: extra}
}

// Exec validates argv against the allowlist and, if it passes, runs kubectl.
// Returns stdout. stderr is included in the returned error on non-zero exit.
//
// argv must NOT include the "kubectl" program name itself; just the args
// starting with the verb (e.g. ["get", "pods", "-A", "-o=json"]).
func (r *Runner) Exec(ctx context.Context, argv []string) ([]byte, error) {
	if err := Validate(argv); err != nil {
		return nil, err
	}
	full := append([]string{}, r.extraArgs...)
	full = append(full, argv...)
	cmd := exec.CommandContext(ctx, r.binary, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Validate is the safety check. Pure function so it can be unit-tested
// without spawning processes.
func Validate(argv []string) error {
	if len(argv) == 0 {
		return &forbiddenReason{reason: "empty argv"}
	}
	verb := argv[0]
	if _, ok := allowedVerbs[verb]; !ok {
		return &forbiddenReason{reason: fmt.Sprintf("verb %q not in allowlist", verb)}
	}

	// Compound verbs: enforce subcommand whitelist.
	switch verb {
	case "config":
		if len(argv) < 2 {
			return &forbiddenReason{reason: "kubectl config requires a subcommand"}
		}
		sub := argv[1]
		if _, bad := forbiddenSubcommands["config"][sub]; bad {
			return &forbiddenReason{reason: fmt.Sprintf("kubectl config %s is forbidden", sub)}
		}
		if _, ok := configAllowedSub[sub]; !ok {
			return &forbiddenReason{reason: fmt.Sprintf("kubectl config %s is not on the allowlist", sub)}
		}
	case "auth":
		if len(argv) < 2 {
			return &forbiddenReason{reason: "kubectl auth requires a subcommand"}
		}
		sub := argv[1]
		if _, bad := forbiddenSubcommands["auth"][sub]; bad {
			return &forbiddenReason{reason: fmt.Sprintf("kubectl auth %s is forbidden", sub)}
		}
		if _, ok := authAllowedSub[sub]; !ok {
			return &forbiddenReason{reason: fmt.Sprintf("kubectl auth %s is not on the allowlist", sub)}
		}
	}

	// Paranoid token sweep: every arg must not contain forbidden substrings.
	for _, a := range argv {
		la := strings.ToLower(a)
		for _, bad := range forbiddenTokens {
			// Match as a whole token or as a prefix of a flag, e.g. "--force=true".
			if la == bad || strings.HasPrefix(la, bad+"=") {
				return &forbiddenReason{reason: fmt.Sprintf("argument %q contains forbidden token %q", a, bad)}
			}
		}
	}
	return nil
}

// --- High-level read-only operations. These are the ONLY entry points the
// rest of the codebase should use. ---

// CurrentContext returns the name of the current kubeconfig context.
func (r *Runner) CurrentContext(ctx context.Context) (string, error) {
	out, err := r.Exec(ctx, []string{"config", "current-context"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ServerVersion returns a short server version string ("vX.Y.Z") or "" on failure.
func (r *Runner) ServerVersion(ctx context.Context) string {
	out, err := r.Exec(ctx, []string{"version", "-o=yaml"})
	if err != nil {
		return ""
	}
	// Trivial scan: find "gitVersion: vX.Y.Z" under serverVersion. We avoid
	// pulling a YAML dependency just for this.
	const key = "gitVersion:"
	idx := strings.LastIndex(string(out), key)
	if idx < 0 {
		return ""
	}
	rest := string(out)[idx+len(key):]
	end := strings.IndexAny(rest, "\r\n")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// ListResourceJSON runs `kubectl get <kind> -A -o=json` (or scoped to a namespace).
// kind is e.g. "pods", "deployments.apps", "events". namespace "" means all namespaces.
func (r *Runner) ListResourceJSON(ctx context.Context, kind, namespace string) ([]byte, error) {
	args := []string{"get", kind, "-o=json", "--ignore-not-found=true"}
	if namespace == "" {
		args = append(args, "-A")
	} else {
		args = append(args, "-n", namespace)
	}
	return r.Exec(ctx, args)
}

// Describe runs `kubectl describe <kind> <name> -n <ns>`. Used to fetch
// non-structured detail only when the user opens a resource in the TUI live
// mode; in replay mode we render describe ourselves from captured data.
func (r *Runner) Describe(ctx context.Context, kind, name, namespace string) ([]byte, error) {
	args := []string{"describe", kind, name}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return r.Exec(ctx, args)
}

// Logs fetches up to tailLines of recent log output from every container in
// a pod (--all-containers --prefix puts container names inline so we can
// store a single record per pod). Streaming is forbidden by the allowlist —
// this always returns a finite snapshot.
func (r *Runner) Logs(ctx context.Context, namespace, pod string, tailLines int) ([]byte, error) {
	if tailLines <= 0 {
		tailLines = 100
	}
	args := []string{
		"logs", pod,
		"-n", namespace,
		"--all-containers=true",
		"--prefix=true",
		fmt.Sprintf("--tail=%d", tailLines),
		"--ignore-errors=true",
	}
	return r.Exec(ctx, args)
}

// CanI checks whether the current credentials may perform a verb on a resource.
// Returns true if "yes", false otherwise. Errors are treated as "no".
func (r *Runner) CanI(ctx context.Context, verb, resource string) bool {
	out, err := r.Exec(ctx, []string{"auth", "can-i", verb, resource, "--all-namespaces"})
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(string(out))) == "yes"
}
