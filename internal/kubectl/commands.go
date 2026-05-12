// Package kubectl is the ONLY place in KronoKube that builds kubectl invocations.
//
// SAFETY CONTRACT
// ---------------
// KronoKube must never mutate the cluster. To make that easy to audit, every
// kubectl command this program may ever execute is listed in this file and
// passes through Runner.Exec, which rejects anything not on the allowlist.
//
// If you are reviewing this codebase to verify the safety claim, you only have
// to read THIS file and runner.go. Any other code that wants to call kubectl
// must use a function exported from this package.
package kubectl

// allowedVerbs is the exhaustive set of kubectl subcommands KronoKube may run.
// All of these are read-only. Adding to this list is the only way to widen
// what the program is allowed to do.
var allowedVerbs = map[string]struct{}{
	"get":      {}, // list/read resources
	"describe": {}, // human-readable resource detail (read-only)
	"version":  {}, // client/server version probe
	"config":   {}, // ONLY the "view" / "current-context" / "get-contexts" subcommands; enforced below
	"api-resources": {}, // discover available kinds (read-only)
	"auth":     {}, // ONLY "can-i" subcommand; enforced below
	"logs":     {}, // tail container logs; read-only. Streaming/--follow is rejected via forbiddenTokens.
}

// forbiddenTokens are substrings that must never appear anywhere in an argv
// passed to kubectl, as a paranoid defense-in-depth check.
//
// They catch:
//   - write verbs we don't list above (apply, create, delete, replace, patch, edit, label,
//     annotate, scale, rollout, cordon, drain, uncordon, taint)
//   - side-effecting subcommands (exec, attach, cp, port-forward, proxy, run, debug,
//     wait, top — top hits metrics-server but is non-mutating; still off by default)
//   - flags that could affect cluster state
var forbiddenTokens = []string{
	"apply", "create", "delete", "replace", "patch", "edit",
	"label", "annotate", "scale", "rollout", "expose",
	"cordon", "uncordon", "drain", "taint",
	"exec", "attach", "cp", "port-forward", "proxy", "run", "debug", "wait",
	"--force", "--grace-period",
	// Streaming flags are read-only but would block the snapshotter
	// indefinitely. KronoKube always fetches finite tails of logs.
	"-f", "--follow",
}

// forbiddenSubcommands enforces that compound verbs only use their read-only
// sub-forms. E.g. "kubectl config view" is fine; "kubectl config set-context" is not.
var forbiddenSubcommands = map[string]map[string]struct{}{
	"config": {
		// allow:
		//   view, current-context, get-contexts, get-clusters, get-users
		// deny everything else:
		"set":            {},
		"set-context":    {},
		"set-cluster":    {},
		"set-credentials": {},
		"unset":          {},
		"delete-context": {},
		"delete-cluster": {},
		"delete-user":    {},
		"rename-context": {},
		"use-context":    {},
	},
	"auth": {
		// allow "can-i" only; deny anything else
		"reconcile": {},
		"whoami":    {}, // harmless but not needed; keep narrow
	},
}

// configAllowedSub is the explicit allowlist for "kubectl config" subcommands.
var configAllowedSub = map[string]struct{}{
	"view":            {},
	"current-context": {},
	"get-contexts":    {},
	"get-clusters":    {},
	"get-users":       {},
}

// authAllowedSub is the explicit allowlist for "kubectl auth" subcommands.
var authAllowedSub = map[string]struct{}{
	"can-i": {},
}
