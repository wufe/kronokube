package tui

import (
	"encoding/json"

	"github.com/wufe/kronokube/internal/model"
	"github.com/wufe/kronokube/internal/store"
)

// Drill-down: pressing Enter on a parent resource (Deployment, StatefulSet,
// DaemonSet, ReplicaSet, Job, CronJob, Node) switches the table to Pods,
// filtered to only the pods that belong to / are scheduled on that parent.
// Esc unwinds back to where the user came from.
//
// Ownership chains:
//
//	Deployment → ReplicaSet → Pod        (two-hop)
//	CronJob    → Job        → Pod        (two-hop)
//	StatefulSet              → Pod        (direct)
//	DaemonSet                → Pod        (direct)
//	ReplicaSet               → Pod        (direct)
//	Job                      → Pod        (direct)
//	Node                     → Pod        (by spec.nodeName)
//
// We match owners by (kind, name) within the same namespace. UID matching
// would be more robust against name collisions but every kind we walk
// either lives in the same namespace as the parent or is cluster-scoped
// with unique names — kind+name is enough.

// drillableKinds is the set of catalog kinds where Enter triggers a drill
// down. For everything else, Enter is a no-op (describe is on 'd').
var drillableKinds = map[model.Kind]struct{}{
	"deployments.apps":  {},
	"statefulsets.apps": {},
	"daemonsets.apps":   {},
	"replicasets.apps":  {},
	"jobs.batch":        {},
	"cronjobs.batch":    {},
	"nodes":             {},
}

func isDrillable(k model.Kind) bool {
	_, ok := drillableKinds[k]
	return ok
}

// drillLabel is the short caption shown in the banner when drill is active.
func drillLabel(k model.Kind) string {
	switch k {
	case "deployments.apps":
		return "Deployment"
	case "statefulsets.apps":
		return "StatefulSet"
	case "daemonsets.apps":
		return "DaemonSet"
	case "replicasets.apps":
		return "ReplicaSet"
	case "jobs.batch":
		return "Job"
	case "cronjobs.batch":
		return "CronJob"
	case "nodes":
		return "Node"
	}
	return string(k)
}

// computePodFilter returns the set of "<namespace>/<name>" keys for pods at
// snapID that belong to (or run on) parentKind/parentNamespace/parentName.
// Returns an empty (non-nil) map if no pods match — distinguishing "no
// matches" from "filter inactive" upstream.
func computePodFilter(s *store.Store, snapID int64, parentKind model.Kind, parentNs, parentName string) (map[string]bool, error) {
	out := make(map[string]bool)
	switch parentKind {
	case "deployments.apps":
		// Two-hop via ReplicaSet.
		ownedRS, err := childrenOwnedBy(s, snapID, "replicasets.apps", parentNs, "Deployment", parentName)
		if err != nil {
			return nil, err
		}
		if err := podsOwnedByAny(s, snapID, parentNs, "ReplicaSet", ownedRS, out); err != nil {
			return nil, err
		}
	case "cronjobs.batch":
		// Two-hop via Job.
		ownedJobs, err := childrenOwnedBy(s, snapID, "jobs.batch", parentNs, "CronJob", parentName)
		if err != nil {
			return nil, err
		}
		if err := podsOwnedByAny(s, snapID, parentNs, "Job", ownedJobs, out); err != nil {
			return nil, err
		}
	case "statefulsets.apps":
		if err := podsOwnedBy(s, snapID, parentNs, "StatefulSet", parentName, out); err != nil {
			return nil, err
		}
	case "daemonsets.apps":
		if err := podsOwnedBy(s, snapID, parentNs, "DaemonSet", parentName, out); err != nil {
			return nil, err
		}
	case "replicasets.apps":
		if err := podsOwnedBy(s, snapID, parentNs, "ReplicaSet", parentName, out); err != nil {
			return nil, err
		}
	case "jobs.batch":
		if err := podsOwnedBy(s, snapID, parentNs, "Job", parentName, out); err != nil {
			return nil, err
		}
	case "nodes":
		// Filter by spec.nodeName == parentName, across all namespaces.
		if err := podsOnNode(s, snapID, parentName, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// resolveResourceBlob returns the resource's blob at snapID, or — if that
// row was stripped by `kk shrink` — any other non-shrunk version of the
// same resource elsewhere in the file. ownerReferences and spec.nodeName
// don't change over a resource's lifetime, so any real copy is a fine
// stand-in for ownership / scheduling questions during a drill-down.
//
// This is what makes the drill view show shrunk-but-owned pods (greyed
// out) alongside their live siblings, instead of silently filtering them
// out.
func resolveResourceBlob(s *store.Store, snapID int64, kind model.Kind, ns, name string) ([]byte, error) {
	raw, err := s.FetchRaw(snapID, kind, ns, name)
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		return raw, nil
	}
	return s.FetchAnyRealBlob(kind, ns, name)
}

// childrenOwnedBy returns the set of names of `childKind` rows in `ns`
// whose ownerReferences contain (ownerKind, ownerName).
func childrenOwnedBy(s *store.Store, snapID int64, childKind model.Kind, ns, ownerKind, ownerName string) (map[string]bool, error) {
	rows, err := s.ResourcesForKind(snapID, childKind, ns)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(rows))
	for _, r := range rows {
		raw, err := resolveResourceBlob(s, snapID, childKind, r.Namespace, r.Name)
		if err != nil || len(raw) == 0 {
			continue
		}
		if isOwnedBy(raw, ownerKind, ownerName) {
			owned[r.Name] = true
		}
	}
	return owned, nil
}

func podsOwnedBy(s *store.Store, snapID int64, ns, ownerKind, ownerName string, out map[string]bool) error {
	rows, err := s.ResourcesForKind(snapID, "pods", ns)
	if err != nil {
		return err
	}
	for _, r := range rows {
		raw, err := resolveResourceBlob(s, snapID, "pods", r.Namespace, r.Name)
		if err != nil || len(raw) == 0 {
			continue
		}
		if isOwnedBy(raw, ownerKind, ownerName) {
			out[r.Namespace+"/"+r.Name] = true
		}
	}
	return nil
}

func podsOwnedByAny(s *store.Store, snapID int64, ns, ownerKind string, ownerNames map[string]bool, out map[string]bool) error {
	if len(ownerNames) == 0 {
		return nil
	}
	rows, err := s.ResourcesForKind(snapID, "pods", ns)
	if err != nil {
		return err
	}
	for _, r := range rows {
		raw, err := resolveResourceBlob(s, snapID, "pods", r.Namespace, r.Name)
		if err != nil || len(raw) == 0 {
			continue
		}
		if isOwnedByAny(raw, ownerKind, ownerNames) {
			out[r.Namespace+"/"+r.Name] = true
		}
	}
	return nil
}

func podsOnNode(s *store.Store, snapID int64, nodeName string, out map[string]bool) error {
	rows, err := s.ResourcesForKind(snapID, "pods", "")
	if err != nil {
		return err
	}
	for _, r := range rows {
		raw, err := resolveResourceBlob(s, snapID, "pods", r.Namespace, r.Name)
		if err != nil || len(raw) == 0 {
			continue
		}
		if podNodeName(raw) == nodeName {
			out[r.Namespace+"/"+r.Name] = true
		}
	}
	return nil
}

// ownerRefShape is the minimal slice of a resource's metadata we need to
// answer ownership questions. JSON decoding into this is cheap.
type ownerRefShape struct {
	Metadata struct {
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

func isOwnedBy(raw []byte, kind, name string) bool {
	var o ownerRefShape
	if json.Unmarshal(raw, &o) != nil {
		return false
	}
	for _, r := range o.Metadata.OwnerReferences {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

func isOwnedByAny(raw []byte, kind string, names map[string]bool) bool {
	var o ownerRefShape
	if json.Unmarshal(raw, &o) != nil {
		return false
	}
	for _, r := range o.Metadata.OwnerReferences {
		if r.Kind == kind && names[r.Name] {
			return true
		}
	}
	return false
}

func podNodeName(raw []byte) string {
	var o struct {
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return ""
	}
	return o.Spec.NodeName
}
