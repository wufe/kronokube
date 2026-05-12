package model

// ResourceDef describes one Kind we know how to capture and tabulate.
type ResourceDef struct {
	// Kind is the kubectl-friendly resource identifier we pass to `kubectl get`.
	Kind Kind
	// DisplayName is what the TUI shows in the kind switcher.
	DisplayName string
	// Namespaced reports whether the resource lives inside a namespace.
	Namespaced bool
	// Columns are the table columns in display order. Each column knows how to
	// pull its cell out of an unstructured resource map.
	Columns []Column
}

// Column declares one column of a tabular view.
type Column struct {
	Title string
	// Width is a target column width (in runes). 0 = auto.
	Width int
	// Extract returns the cell text for a single resource object (decoded JSON).
	Extract func(obj map[string]any) string
}

// Catalog is the ordered list of resource kinds KronoKube captures by default.
// Order is the order shown in the TUI's kind switcher.
//
// Editing this list is the *only* way to change what gets captured. There is
// intentionally no auto-discovery of CRDs — see project requirements.
var Catalog = []ResourceDef{
	defPods(),
	defDeployments(),
	defReplicaSets(),
	defStatefulSets(),
	defDaemonSets(),
	defJobs(),
	defCronJobs(),
	defServices(),
	defEndpointSlices(),
	defIngresses(),
	defNetworkPolicies(),
	defHPAs(),
	defPDBs(),
	defServiceAccounts(),
	defNodes(),
	defNamespaces(),
	// Events is captured but not shown as just-another-kind; it has its own
	// view in the TUI. Still part of the catalog so capture treats it uniformly.
	defEvents(),
}

// CatalogByKind returns a quick-lookup map.
func CatalogByKind() map[Kind]ResourceDef {
	m := make(map[Kind]ResourceDef, len(Catalog))
	for _, d := range Catalog {
		m[d.Kind] = d
	}
	return m
}
