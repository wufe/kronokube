package capture

import (
	"testing"

	"github.com/wufe/kronokube/internal/model"
)

func TestTabulate_Pods(t *testing.T) {
	json := []byte(`{
		"items": [
			{
				"metadata": {"name": "p1", "namespace": "default", "uid": "u1", "creationTimestamp": "2020-01-01T00:00:00Z"},
				"spec": {"nodeName": "node-a", "containers": [{"name":"c1"},{"name":"c2"}]},
				"status": {
					"phase": "Running",
					"podIP": "10.0.0.1",
					"containerStatuses": [
						{"ready": true, "restartCount": 0, "state":{"running":{}}},
						{"ready": true, "restartCount": 1, "state":{"running":{}}}
					]
				}
			},
			{
				"metadata": {"name": "p2", "namespace": "kube-system", "uid":"u2", "creationTimestamp": "2020-01-01T00:00:00Z"},
				"spec": {"containers": [{"name":"c1"}]},
				"status": {
					"phase": "Pending",
					"containerStatuses": [
						{"ready": false, "restartCount": 5, "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
					]
				}
			}
		]
	}`)
	var def model.ResourceDef
	for _, d := range model.Catalog {
		if d.Kind == "pods" {
			def = d
			break
		}
	}
	rows, err := Tabulate(def, json)
	if err != nil {
		t.Fatalf("Tabulate: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	// Find columns by header so the test doesn't break if ordering shifts.
	colIdx := map[string]int{}
	for i, c := range def.Columns {
		colIdx[c.Title] = i
	}

	if got := rows[0].Cells[colIdx["READY"]]; got != "2/2" {
		t.Errorf("p1 READY = %q, want 2/2", got)
	}
	if got := rows[0].Cells[colIdx["RESTARTS"]]; got != "1" {
		t.Errorf("p1 RESTARTS = %q, want 1", got)
	}
	if got := rows[1].Cells[colIdx["STATUS"]]; got != "CrashLoopBackOff" {
		t.Errorf("p2 STATUS = %q, want CrashLoopBackOff", got)
	}
	if got := rows[1].Cells[colIdx["RESTARTS"]]; got != "5" {
		t.Errorf("p2 RESTARTS = %q, want 5", got)
	}
}

func TestTabulate_EmptyList(t *testing.T) {
	def := model.Catalog[0]
	rows, err := Tabulate(def, []byte(`{"items":[]}`))
	if err != nil {
		t.Fatalf("Tabulate: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

func TestTabulateEvents(t *testing.T) {
	json := []byte(`{
		"items": [
			{
				"metadata": {"name":"e1", "namespace":"default"},
				"type":"Warning",
				"reason":"BackOff",
				"message":"Back-off restarting failed container",
				"count": 5,
				"lastTimestamp": "2024-01-01T00:00:00Z",
				"involvedObject": {"kind":"Pod","name":"p2","uid":"u2"}
			}
		]
	}`)
	evs, err := TabulateEvents(json)
	if err != nil {
		t.Fatalf("TabulateEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].Reason != "BackOff" || evs[0].Type != "Warning" || evs[0].Count != 5 {
		t.Errorf("event mismatch: %+v", evs[0])
	}
	if evs[0].Object != "Pod/p2" {
		t.Errorf("object = %q, want Pod/p2", evs[0].Object)
	}
}
