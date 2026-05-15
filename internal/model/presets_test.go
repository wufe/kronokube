package model

import (
	"strings"
	"testing"
)

func TestResolveKinds_DefaultPresetExcludesHeavyKinds(t *testing.T) {
	got, err := ResolveKinds("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range got {
		if k == "endpointslices.discovery.k8s.io" || k == "replicasets.apps" {
			t.Fatalf("default preset should not include %s; got full list: %v", k, got)
		}
	}
	if len(got) != len(Catalog)-2 {
		t.Fatalf("default preset expected len(Catalog)-2 = %d, got %d", len(Catalog)-2, len(got))
	}
}

func TestResolveKinds_FullPresetIsEverything(t *testing.T) {
	got, err := ResolveKinds("full", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(Catalog) {
		t.Fatalf("full preset expected len(Catalog) = %d, got %d", len(Catalog), len(got))
	}
}

func TestResolveKinds_ExcludeKindsRemovesThem(t *testing.T) {
	got, err := ResolveKinds("full", []string{"events", "nodes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range got {
		if k == "events" || k == "nodes" {
			t.Fatalf("exclude-kinds should have removed %s; got %v", k, got)
		}
	}
}

func TestResolveKinds_ExplicitListWithShortNames(t *testing.T) {
	got, err := ResolveKinds("pods,deployments", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[Kind]bool{"pods": true, "deployments.apps": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d kinds, got %d (%v)", len(want), len(got), got)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("unexpected kind in result: %s", k)
		}
	}
}

func TestResolveKinds_UnknownKindIsError(t *testing.T) {
	_, err := ResolveKinds("bananas", nil)
	if err == nil {
		t.Fatalf("expected error for unknown kind, got nil")
	}
	if !strings.Contains(err.Error(), "bananas") {
		t.Fatalf("error should mention the bad token; got %q", err.Error())
	}
}

func TestResolveKinds_EmptyResolvedSetIsError(t *testing.T) {
	// Take the minimal preset and exclude everything in it.
	excludes := []string{"pods", "deployments", "statefulsets", "services", "events"}
	_, err := ResolveKinds("minimal", excludes)
	if err == nil {
		t.Fatalf("expected error for empty resolved set, got nil")
	}
}

func TestResolveKinds_OrderFollowsCatalog(t *testing.T) {
	got, err := ResolveKinds("full", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, d := range Catalog {
		if got[i] != d.Kind {
			t.Fatalf("at index %d expected catalog kind %s, got %s", i, d.Kind, got[i])
		}
	}
}
