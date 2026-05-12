// Package capture takes raw kubectl JSON output and produces the typed Rows
// + Events that the store layer persists. It contains no kubectl invocation
// logic itself — that lives in internal/kubectl.
package capture

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wufe/kronokube/internal/model"
)

// Tabulate converts a `kubectl get <kind> -o=json` blob into Rows, applying
// the column definitions for that Kind from model.Catalog.
//
// Returns (nil, nil) if the JSON is an empty list. Items lacking expected
// fields produce rows with "" cells rather than errors — the catch-all is
// "best effort tabulation", matching how k9s behaves.
func Tabulate(def model.ResourceDef, raw []byte) ([]model.Row, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	rows := make([]model.Row, 0, len(doc.Items))
	for _, item := range doc.Items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		cells := make([]string, len(def.Columns))
		for i, c := range def.Columns {
			cells[i] = c.Extract(obj)
		}
		row := model.Row{
			Kind:      def.Kind,
			Namespace: extractString(obj, "metadata", "namespace"),
			Name:      extractString(obj, "metadata", "name"),
			UID:       extractString(obj, "metadata", "uid"),
			Cells:     cells,
			RawJSON:   []byte(item),
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// TabulateEvents is a specialized parser for the events list that produces
// model.Event records (in addition to its Row representation, which Tabulate
// already provides). The event records support the per-resource events view
// without re-parsing JSON at render time.
func TabulateEvents(raw []byte) ([]model.Event, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	out := make([]model.Event, 0, len(doc.Items))
	for _, it := range doc.Items {
		ev := model.Event{
			Namespace:      extractString(it, "metadata", "namespace"),
			Name:           extractString(it, "metadata", "name"),
			Type:           extractString(it, "type"),
			Reason:         extractString(it, "reason"),
			Message:        extractString(it, "message"),
			Object:         fmt.Sprintf("%s/%s", extractString(it, "involvedObject", "kind"), extractString(it, "involvedObject", "name")),
			ObjectUID:      extractString(it, "involvedObject", "uid"),
			LastTimestamp:  parseTime(extractString(it, "lastTimestamp"), extractString(it, "eventTime")),
			FirstTimestamp: parseTime(extractString(it, "firstTimestamp"), extractString(it, "eventTime")),
		}
		if c, ok := walk(it, "count").(float64); ok {
			ev.Count = int32(c)
		}
		out = append(out, ev)
	}
	return out, nil
}

func extractString(obj map[string]any, path ...string) string {
	v := walk(obj, path...)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func walk(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func parseTime(primary, fallback string) time.Time {
	for _, s := range []string{primary, fallback} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
