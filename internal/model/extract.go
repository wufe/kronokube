package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// --- generic helpers to walk an unstructured Kubernetes object safely ---

// pathStr returns obj[a][b][c]... as a string, or "" if any step is missing.
func pathStr(obj map[string]any, path ...string) string {
	v := walk(obj, path...)
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers are float64; trim if integer-ish.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
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

// pathInt is like pathStr but returns int; 0 on miss.
func pathInt(obj map[string]any, path ...string) int {
	v := walk(obj, path...)
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}

// pathSlice returns a []any at path, or nil.
func pathSlice(obj map[string]any, path ...string) []any {
	v := walk(obj, path...)
	s, _ := v.([]any)
	return s
}

// age returns a humanized duration since the given RFC3339 timestamp, k9s-style:
// "3d", "5h", "12m", "37s".
func age(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

func metaName(obj map[string]any) string      { return pathStr(obj, "metadata", "name") }
func metaNamespace(obj map[string]any) string { return pathStr(obj, "metadata", "namespace") }
func metaAge(obj map[string]any) string       { return age(pathStr(obj, "metadata", "creationTimestamp")) }

// --- per-kind definitions ---

func defPods() ResourceDef {
	return ResourceDef{
		Kind: "pods", DisplayName: "Pods", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "READY", Extract: podReady},
			{Title: "STATUS", Extract: podStatus},
			{Title: "RESTARTS", Extract: podRestarts},
			{Title: "IP", Extract: func(o map[string]any) string { return pathStr(o, "status", "podIP") }},
			{Title: "NODE", Extract: func(o map[string]any) string { return pathStr(o, "spec", "nodeName") }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func podReady(o map[string]any) string {
	cs := pathSlice(o, "status", "containerStatuses")
	total := len(pathSlice(o, "spec", "containers"))
	ready := 0
	for _, c := range cs {
		m, _ := c.(map[string]any)
		if b, _ := m["ready"].(bool); b {
			ready++
		}
	}
	if total == 0 {
		total = len(cs)
	}
	return fmt.Sprintf("%d/%d", ready, total)
}

func podStatus(o map[string]any) string {
	// Prefer the most-specific reason from container waiting states; fall back to phase.
	cs := pathSlice(o, "status", "containerStatuses")
	for _, c := range cs {
		m, _ := c.(map[string]any)
		st, _ := m["state"].(map[string]any)
		if w, ok := st["waiting"].(map[string]any); ok {
			if r, _ := w["reason"].(string); r != "" {
				return r
			}
		}
		if w, ok := st["terminated"].(map[string]any); ok {
			if r, _ := w["reason"].(string); r != "" {
				return r
			}
		}
	}
	if r := pathStr(o, "status", "reason"); r != "" {
		return r
	}
	return pathStr(o, "status", "phase")
}

func podRestarts(o map[string]any) string {
	cs := pathSlice(o, "status", "containerStatuses")
	total := 0
	for _, c := range cs {
		m, _ := c.(map[string]any)
		if v, ok := m["restartCount"].(float64); ok {
			total += int(v)
		}
	}
	return strconv.Itoa(total)
}

func defDeployments() ResourceDef {
	return ResourceDef{
		Kind: "deployments.apps", DisplayName: "Deployments", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "READY", Extract: func(o map[string]any) string {
				return fmt.Sprintf("%d/%d", pathInt(o, "status", "readyReplicas"), pathInt(o, "spec", "replicas"))
			}},
			{Title: "UP-TO-DATE", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "updatedReplicas")) }},
			{Title: "AVAILABLE", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "availableReplicas")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defReplicaSets() ResourceDef {
	return ResourceDef{
		Kind: "replicasets.apps", DisplayName: "ReplicaSets", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "DESIRED", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "spec", "replicas")) }},
			{Title: "CURRENT", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "replicas")) }},
			{Title: "READY", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "readyReplicas")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defStatefulSets() ResourceDef {
	return ResourceDef{
		Kind: "statefulsets.apps", DisplayName: "StatefulSets", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "READY", Extract: func(o map[string]any) string {
				return fmt.Sprintf("%d/%d", pathInt(o, "status", "readyReplicas"), pathInt(o, "spec", "replicas"))
			}},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defDaemonSets() ResourceDef {
	return ResourceDef{
		Kind: "daemonsets.apps", DisplayName: "DaemonSets", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "DESIRED", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "desiredNumberScheduled")) }},
			{Title: "CURRENT", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "currentNumberScheduled")) }},
			{Title: "READY", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "numberReady")) }},
			{Title: "UP-TO-DATE", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "updatedNumberScheduled")) }},
			{Title: "AVAILABLE", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "numberAvailable")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defJobs() ResourceDef {
	return ResourceDef{
		Kind: "jobs.batch", DisplayName: "Jobs", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "COMPLETIONS", Extract: func(o map[string]any) string {
				return fmt.Sprintf("%d/%d", pathInt(o, "status", "succeeded"), pathInt(o, "spec", "completions"))
			}},
			{Title: "DURATION", Extract: jobDuration},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func jobDuration(o map[string]any) string {
	start := pathStr(o, "status", "startTime")
	end := pathStr(o, "status", "completionTime")
	if start == "" {
		return ""
	}
	st, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return ""
	}
	var et time.Time
	if end != "" {
		et, _ = time.Parse(time.RFC3339, end)
	} else {
		et = time.Now()
	}
	d := et.Sub(st)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}

func defCronJobs() ResourceDef {
	return ResourceDef{
		Kind: "cronjobs.batch", DisplayName: "CronJobs", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "SCHEDULE", Extract: func(o map[string]any) string { return pathStr(o, "spec", "schedule") }},
			{Title: "SUSPEND", Extract: func(o map[string]any) string {
				v := walk(o, "spec", "suspend")
				if b, ok := v.(bool); ok {
					return strconv.FormatBool(b)
				}
				return "false"
			}},
			{Title: "ACTIVE", Extract: func(o map[string]any) string { return strconv.Itoa(len(pathSlice(o, "status", "active"))) }},
			{Title: "LAST-SCHEDULE", Extract: func(o map[string]any) string { return age(pathStr(o, "status", "lastScheduleTime")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defServices() ResourceDef {
	return ResourceDef{
		Kind: "services", DisplayName: "Services", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "TYPE", Extract: func(o map[string]any) string { return pathStr(o, "spec", "type") }},
			{Title: "CLUSTER-IP", Extract: func(o map[string]any) string { return pathStr(o, "spec", "clusterIP") }},
			{Title: "EXTERNAL-IP", Extract: svcExternalIP},
			{Title: "PORT(S)", Extract: svcPorts},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func svcExternalIP(o map[string]any) string {
	ing := pathSlice(o, "status", "loadBalancer", "ingress")
	var ips []string
	for _, e := range ing {
		m, _ := e.(map[string]any)
		if ip, _ := m["ip"].(string); ip != "" {
			ips = append(ips, ip)
		}
		if h, _ := m["hostname"].(string); h != "" {
			ips = append(ips, h)
		}
	}
	if len(ips) == 0 {
		if eips := pathSlice(o, "spec", "externalIPs"); len(eips) > 0 {
			for _, e := range eips {
				if s, ok := e.(string); ok {
					ips = append(ips, s)
				}
			}
		}
	}
	if len(ips) == 0 {
		return "<none>"
	}
	return strings.Join(ips, ",")
}

func svcPorts(o map[string]any) string {
	ports := pathSlice(o, "spec", "ports")
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		m, _ := p.(map[string]any)
		port := pathInt(m, "port")
		proto := pathStr(m, "protocol")
		if proto == "" {
			proto = "TCP"
		}
		if np := pathInt(m, "nodePort"); np > 0 {
			out = append(out, fmt.Sprintf("%d:%d/%s", port, np, proto))
		} else {
			out = append(out, fmt.Sprintf("%d/%s", port, proto))
		}
	}
	return strings.Join(out, ",")
}

func defEndpointSlices() ResourceDef {
	return ResourceDef{
		Kind: "endpointslices.discovery.k8s.io", DisplayName: "EndpointSlices", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "ADDRESSTYPE", Extract: func(o map[string]any) string { return pathStr(o, "addressType") }},
			{Title: "PORTS", Extract: epsPorts},
			{Title: "ENDPOINTS", Extract: epsEndpoints},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func epsPorts(o map[string]any) string {
	ports := pathSlice(o, "ports")
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		m, _ := p.(map[string]any)
		out = append(out, strconv.Itoa(pathInt(m, "port")))
	}
	return strings.Join(out, ",")
}

func epsEndpoints(o map[string]any) string {
	eps := pathSlice(o, "endpoints")
	var addrs []string
	for _, e := range eps {
		m, _ := e.(map[string]any)
		for _, a := range pathSlice(m, "addresses") {
			if s, ok := a.(string); ok {
				addrs = append(addrs, s)
			}
		}
	}
	if len(addrs) == 0 {
		return "<none>"
	}
	if len(addrs) > 3 {
		return strings.Join(addrs[:3], ",") + fmt.Sprintf(" +%d", len(addrs)-3)
	}
	return strings.Join(addrs, ",")
}

func defIngresses() ResourceDef {
	return ResourceDef{
		Kind: "ingresses.networking.k8s.io", DisplayName: "Ingresses", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "CLASS", Extract: func(o map[string]any) string { return pathStr(o, "spec", "ingressClassName") }},
			{Title: "HOSTS", Extract: ingHosts},
			{Title: "ADDRESS", Extract: ingAddress},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func ingHosts(o map[string]any) string {
	rules := pathSlice(o, "spec", "rules")
	var hosts []string
	for _, r := range rules {
		m, _ := r.(map[string]any)
		if h, _ := m["host"].(string); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	return strings.Join(hosts, ",")
}

func ingAddress(o map[string]any) string {
	ing := pathSlice(o, "status", "loadBalancer", "ingress")
	var out []string
	for _, e := range ing {
		m, _ := e.(map[string]any)
		if ip, _ := m["ip"].(string); ip != "" {
			out = append(out, ip)
		} else if h, _ := m["hostname"].(string); h != "" {
			out = append(out, h)
		}
	}
	return strings.Join(out, ",")
}

func defNetworkPolicies() ResourceDef {
	return ResourceDef{
		Kind: "networkpolicies.networking.k8s.io", DisplayName: "NetworkPolicies", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "POD-SELECTOR", Extract: func(o map[string]any) string {
				ml := walk(o, "spec", "podSelector", "matchLabels")
				m, _ := ml.(map[string]any)
				if len(m) == 0 {
					return "<all>"
				}
				parts := make([]string, 0, len(m))
				for k, v := range m {
					parts = append(parts, fmt.Sprintf("%s=%v", k, v))
				}
				return strings.Join(parts, ",")
			}},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defHPAs() ResourceDef {
	return ResourceDef{
		Kind: "horizontalpodautoscalers.autoscaling", DisplayName: "HPAs", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "REFERENCE", Extract: func(o map[string]any) string {
				return fmt.Sprintf("%s/%s", pathStr(o, "spec", "scaleTargetRef", "kind"), pathStr(o, "spec", "scaleTargetRef", "name"))
			}},
			{Title: "MIN", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "spec", "minReplicas")) }},
			{Title: "MAX", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "spec", "maxReplicas")) }},
			{Title: "REPLICAS", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "currentReplicas")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defPDBs() ResourceDef {
	return ResourceDef{
		Kind: "poddisruptionbudgets.policy", DisplayName: "PDBs", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "MIN-AVAILABLE", Extract: func(o map[string]any) string { return pathStr(o, "spec", "minAvailable") }},
			{Title: "MAX-UNAVAILABLE", Extract: func(o map[string]any) string { return pathStr(o, "spec", "maxUnavailable") }},
			{Title: "ALLOWED-DISRUPTIONS", Extract: func(o map[string]any) string { return strconv.Itoa(pathInt(o, "status", "disruptionsAllowed")) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defServiceAccounts() ResourceDef {
	return ResourceDef{
		Kind: "serviceaccounts", DisplayName: "ServiceAccounts", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "NAME", Extract: metaName},
			{Title: "SECRETS", Extract: func(o map[string]any) string { return strconv.Itoa(len(pathSlice(o, "secrets"))) }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defNodes() ResourceDef {
	return ResourceDef{
		Kind: "nodes", DisplayName: "Nodes", Namespaced: false,
		Columns: []Column{
			{Title: "NAME", Extract: metaName},
			{Title: "STATUS", Extract: nodeStatus},
			{Title: "ROLES", Extract: nodeRoles},
			{Title: "AGE", Extract: metaAge},
			{Title: "VERSION", Extract: func(o map[string]any) string { return pathStr(o, "status", "nodeInfo", "kubeletVersion") }},
			{Title: "INTERNAL-IP", Extract: nodeInternalIP},
			{Title: "OS-IMAGE", Extract: func(o map[string]any) string { return pathStr(o, "status", "nodeInfo", "osImage") }},
			{Title: "KERNEL", Extract: func(o map[string]any) string { return pathStr(o, "status", "nodeInfo", "kernelVersion") }},
			{Title: "RUNTIME", Extract: func(o map[string]any) string { return pathStr(o, "status", "nodeInfo", "containerRuntimeVersion") }},
		},
	}
}

func nodeStatus(o map[string]any) string {
	conds := pathSlice(o, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if pathStr(m, "type") == "Ready" {
			if pathStr(m, "status") == "True" {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func nodeRoles(o map[string]any) string {
	labels := walk(o, "metadata", "labels")
	m, _ := labels.(map[string]any)
	var roles []string
	for k := range m {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(k, "node-role.kubernetes.io/")
			if role != "" {
				roles = append(roles, role)
			}
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	return strings.Join(roles, ",")
}

func nodeInternalIP(o map[string]any) string {
	addrs := pathSlice(o, "status", "addresses")
	for _, a := range addrs {
		m, _ := a.(map[string]any)
		if pathStr(m, "type") == "InternalIP" {
			return pathStr(m, "address")
		}
	}
	return ""
}

func defNamespaces() ResourceDef {
	return ResourceDef{
		Kind: "namespaces", DisplayName: "Namespaces", Namespaced: false,
		Columns: []Column{
			{Title: "NAME", Extract: metaName},
			{Title: "STATUS", Extract: func(o map[string]any) string { return pathStr(o, "status", "phase") }},
			{Title: "AGE", Extract: metaAge},
		},
	}
}

func defEvents() ResourceDef {
	return ResourceDef{
		Kind: "events", DisplayName: "Events", Namespaced: true,
		Columns: []Column{
			{Title: "NAMESPACE", Extract: metaNamespace},
			{Title: "LAST-SEEN", Extract: func(o map[string]any) string {
				if v := pathStr(o, "lastTimestamp"); v != "" {
					return age(v)
				}
				return age(pathStr(o, "eventTime"))
			}},
			{Title: "TYPE", Extract: func(o map[string]any) string { return pathStr(o, "type") }},
			{Title: "REASON", Extract: func(o map[string]any) string { return pathStr(o, "reason") }},
			{Title: "OBJECT", Extract: func(o map[string]any) string {
				return fmt.Sprintf("%s/%s", pathStr(o, "involvedObject", "kind"), pathStr(o, "involvedObject", "name"))
			}},
			{Title: "MESSAGE", Extract: func(o map[string]any) string { return pathStr(o, "message") }},
		},
	}
}
