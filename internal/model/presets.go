package model

import (
	"fmt"
	"sort"
	"strings"
)

// Kind preset names accepted by --kinds.
const (
	PresetMinimal   = "minimal"
	PresetDefault   = "default"
	PresetWorkloads = "workloads"
	PresetFull      = "full"
)

// presets maps preset name to the kinds it expands to. PresetFull is computed
// from Catalog at resolve time so adding a Kind to the catalog automatically
// joins the full preset.
var presets = map[string][]Kind{
	PresetMinimal: {
		"pods",
		"deployments.apps",
		"statefulsets.apps",
		"services",
		"events",
	},
	PresetWorkloads: {
		"pods",
		"deployments.apps",
		"replicasets.apps",
		"statefulsets.apps",
		"daemonsets.apps",
		"jobs.batch",
		"cronjobs.batch",
	},
}

// presetDefaultExcludes is the set of kinds dropped from PresetFull to form
// PresetDefault. These are the kinds that scale fastest with cluster size and
// are the most common capture-time bottleneck.
var presetDefaultExcludes = map[Kind]bool{
	"endpointslices.discovery.k8s.io": true,
	"replicasets.apps":                true,
}

// PresetNames returns the recognized preset names in display order.
func PresetNames() []string {
	return []string{PresetMinimal, PresetDefault, PresetWorkloads, PresetFull}
}

// IsPreset reports whether name is a recognized preset.
func IsPreset(name string) bool {
	switch name {
	case PresetMinimal, PresetDefault, PresetWorkloads, PresetFull:
		return true
	}
	return false
}

// ResolveKinds turns a user-supplied --kinds spec (preset name or
// comma-separated kind list) and an --exclude-kinds list into the final
// ordered slice of Kind to capture. Order follows Catalog so the TUI's
// kind-switcher stays predictable.
//
// Unknown preset names, unknown kinds in either list, and a fully empty
// resolved set all return an error so the user finds out at flag-parse time.
func ResolveKinds(spec string, excludes []string) ([]Kind, error) {
	want, err := expandSpec(spec)
	if err != nil {
		return nil, err
	}
	excludeSet, err := normalizeKindList(excludes, "exclude-kinds")
	if err != nil {
		return nil, err
	}
	wantSet := make(map[Kind]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for k := range excludeSet {
		delete(wantSet, k)
	}

	// Preserve catalog order.
	out := make([]Kind, 0, len(wantSet))
	for _, d := range Catalog {
		if wantSet[d.Kind] {
			out = append(out, d.Kind)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--kinds / --exclude-kinds resolved to zero kinds")
	}
	return out, nil
}

// expandSpec resolves the --kinds argument to a set of canonical Kind values.
// Empty spec is treated as PresetDefault.
func expandSpec(spec string) ([]Kind, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = PresetDefault
	}
	// Preset name (must be a single token, no commas).
	if !strings.Contains(spec, ",") && IsPreset(spec) {
		return expandPreset(spec), nil
	}
	// Otherwise treat as a comma-separated kind list.
	parts := strings.Split(spec, ",")
	set, err := normalizeKindList(parts, "kinds")
	if err != nil {
		return nil, err
	}
	out := make([]Kind, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out, nil
}

func expandPreset(name string) []Kind {
	switch name {
	case PresetFull:
		out := make([]Kind, 0, len(Catalog))
		for _, d := range Catalog {
			out = append(out, d.Kind)
		}
		return out
	case PresetDefault:
		out := make([]Kind, 0, len(Catalog))
		for _, d := range Catalog {
			if presetDefaultExcludes[d.Kind] {
				continue
			}
			out = append(out, d.Kind)
		}
		return out
	default:
		// Static preset list.
		cp := make([]Kind, len(presets[name]))
		copy(cp, presets[name])
		return cp
	}
}

// normalizeKindList parses a list of user-supplied kind tokens (catalog name
// or short prefix before the first dot) into a set of canonical Kinds.
// Unknown or ambiguous tokens return an error naming the flag.
func normalizeKindList(raw []string, flagName string) (map[Kind]bool, error) {
	out := map[Kind]bool{}
	short := shortNameIndex()
	full := map[Kind]bool{}
	for _, d := range Catalog {
		full[d.Kind] = true
	}
	for _, tok := range raw {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if full[Kind(tok)] {
			out[Kind(tok)] = true
			continue
		}
		if matches, ok := short[tok]; ok {
			if len(matches) > 1 {
				return nil, fmt.Errorf("--%s: %q is ambiguous; matches %s", flagName, tok, kindsToString(matches))
			}
			out[matches[0]] = true
			continue
		}
		return nil, fmt.Errorf("--%s: unknown kind %q; valid kinds: %s", flagName, tok, validKindHint())
	}
	return out, nil
}

// shortNameIndex maps the part before the first dot to the catalog Kinds that
// share that short form. Used to accept "deployments" as shorthand for
// "deployments.apps". Multi-match is preserved as an error in the caller.
func shortNameIndex() map[string][]Kind {
	out := map[string][]Kind{}
	for _, d := range Catalog {
		s := string(d.Kind)
		if i := strings.IndexByte(s, '.'); i > 0 {
			short := s[:i]
			out[short] = append(out[short], d.Kind)
		}
	}
	return out
}

func validKindHint() string {
	names := make([]string, 0, len(Catalog))
	for _, d := range Catalog {
		names = append(names, string(d.Kind))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func kindsToString(ks []Kind) string {
	s := make([]string, len(ks))
	for i, k := range ks {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}
