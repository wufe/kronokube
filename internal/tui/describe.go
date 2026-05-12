package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wufe/kronokube/internal/model"
	"gopkg.in/yaml.v3"
)

// renderDescribe builds a kubectl describe-style view from the raw JSON of
// a single resource and the events targeting it at the same snapshot.
//
// The output is plain text styled with lipgloss. The caller is responsible
// for hosting it in a viewport for scrolling.
func renderDescribe(kind model.Kind, ns, name string, raw []byte, events []model.Event) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("%s  %s/%s", kind, ns, name)))
	b.WriteString("\n\n")

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		b.WriteString(StyleError.Render("cannot decode resource JSON: " + err.Error()))
		return b.String()
	}

	writeHeader(&b, obj)
	writeLabelsAnnotations(&b, obj)
	writeStatus(&b, obj)
	writeSpec(&b, obj)
	writeEvents(&b, events)
	return b.String()
}

func writeHeader(b *strings.Builder, obj map[string]any) {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		return
	}
	fmt.Fprintf(b, "%s %s\n", StyleHeader.Render("Name:"), str(m["name"]))
	if ns := str(m["namespace"]); ns != "" {
		fmt.Fprintf(b, "%s %s\n", StyleHeader.Render("Namespace:"), ns)
	}
	if uid := str(m["uid"]); uid != "" {
		fmt.Fprintf(b, "%s %s\n", StyleHeader.Render("UID:"), uid)
	}
	if ct := str(m["creationTimestamp"]); ct != "" {
		fmt.Fprintf(b, "%s %s\n", StyleHeader.Render("Created:"), ct)
	}
	b.WriteString("\n")
}

func writeLabelsAnnotations(b *strings.Builder, obj map[string]any) {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		return
	}
	if labels, ok := m["labels"].(map[string]any); ok && len(labels) > 0 {
		b.WriteString(StyleHeader.Render("Labels:") + "\n")
		writeKVMap(b, labels, "  ")
	}
	if anns, ok := m["annotations"].(map[string]any); ok && len(anns) > 0 {
		b.WriteString(StyleHeader.Render("Annotations:") + "\n")
		writeKVMap(b, anns, "  ")
	}
}

func writeKVMap(b *strings.Builder, m map[string]any, indent string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := str(m[k])
		if len(v) > 120 {
			v = v[:117] + "..."
		}
		fmt.Fprintf(b, "%s%s=%s\n", indent, k, v)
	}
	b.WriteString("\n")
}

func writeStatus(b *strings.Builder, obj map[string]any) {
	st, _ := obj["status"].(map[string]any)
	if len(st) == 0 {
		return
	}
	b.WriteString(StyleHeader.Render("Status:") + "\n")
	// Pretty-print as YAML for readability.
	y, err := yaml.Marshal(st)
	if err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(y), "\n"), "\n") {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
}

func writeSpec(b *strings.Builder, obj map[string]any) {
	sp, _ := obj["spec"].(map[string]any)
	if len(sp) == 0 {
		return
	}
	b.WriteString(StyleHeader.Render("Spec:") + "\n")
	y, err := yaml.Marshal(sp)
	if err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(y), "\n"), "\n") {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
}

func writeEvents(b *strings.Builder, events []model.Event) {
	b.WriteString(StyleHeader.Render(fmt.Sprintf("Events: (%d)", len(events))) + "\n")
	if len(events) == 0 {
		b.WriteString(StyleMuted.Render("  <none>") + "\n")
		return
	}
	for _, e := range events {
		t := e.Type
		styled := t
		if t == "Warning" {
			styled = StyleWarn.Render(t)
		} else if t == "Normal" {
			styled = StyleOK.Render(t)
		}
		ts := ""
		if !e.LastTimestamp.IsZero() {
			ts = e.LastTimestamp.Format(time.RFC3339)
		}
		fmt.Fprintf(b, "  %s [%s] %s ×%d — %s\n", styled, ts, e.Reason, e.Count, e.Message)
	}
}

// renderResourceYAML pretty-prints the captured JSON of a resource as YAML.
// Top-level keys are ordered the way kubectl get -o yaml prints them
// (apiVersion, kind, metadata, spec, status, then anything else), so the
// output is easy to read alongside a real kubectl session. Nested keys are
// alphabetical, which is yaml.v3's default — close enough for debugging.
func renderResourceYAML(kind model.Kind, ns, name string, raw []byte) string {
	var b strings.Builder
	b.WriteString(StyleTitle.Render(fmt.Sprintf("YAML — %s  %s/%s", kind, ns, name)))
	b.WriteString("\n\n")
	if len(raw) == 0 {
		b.WriteString(StyleMuted.Render("(no captured data for this resource at this snapshot)"))
		return b.String()
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		b.WriteString(StyleError.Render("cannot decode resource JSON: " + err.Error()))
		return b.String()
	}
	out, err := marshalKubectlYAML(obj)
	if err != nil {
		b.WriteString(StyleError.Render("yaml marshal: " + err.Error()))
		return b.String()
	}
	b.Write(out)
	return b.String()
}

// kubectlYAMLOrder is the top-level key sequence kubectl uses in -o yaml.
var kubectlYAMLOrder = []string{"apiVersion", "kind", "metadata", "spec", "status"}

func marshalKubectlYAML(obj map[string]any) ([]byte, error) {
	// Build a yaml.Node mapping with explicit key order.
	root := &yaml.Node{Kind: yaml.MappingNode}
	emitted := map[string]bool{}
	for _, k := range kubectlYAMLOrder {
		if v, ok := obj[k]; ok {
			if err := appendKV(root, k, v); err != nil {
				return nil, err
			}
			emitted[k] = true
		}
	}
	// Remaining keys in alphabetical order for determinism.
	rest := make([]string, 0, len(obj))
	for k := range obj {
		if !emitted[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if err := appendKV(root, k, obj[k]); err != nil {
			return nil, err
		}
	}
	return yaml.Marshal(root)
}

func appendKV(parent *yaml.Node, key string, value any) error {
	kn := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	vn := &yaml.Node{}
	if err := vn.Encode(value); err != nil {
		return err
	}
	parent.Content = append(parent.Content, kn, vn)
	return nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
