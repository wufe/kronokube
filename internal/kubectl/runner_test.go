package kubectl

import (
	"strings"
	"testing"
)

func TestValidate_AllowsReadOnly(t *testing.T) {
	cases := [][]string{
		{"get", "pods", "-A", "-o=json"},
		{"get", "deployments.apps", "-n", "kube-system", "-o=json"},
		{"describe", "deployment", "foo", "-n", "ns"},
		{"version", "-o=yaml"},
		{"config", "current-context"},
		{"config", "view"},
		{"config", "get-contexts"},
		{"api-resources"},
		{"auth", "can-i", "list", "pods", "--all-namespaces"},
		{"logs", "p1", "-n", "default", "--all-containers=true", "--prefix=true", "--tail=100"},
	}
	for _, c := range cases {
		if err := Validate(c); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", c, err)
		}
	}
}

func TestValidate_RejectsWrites(t *testing.T) {
	cases := [][]string{
		{"apply", "-f", "x.yaml"},
		{"create", "deployment", "foo", "--image=nginx"},
		{"delete", "pod", "foo"},
		{"replace", "-f", "x.yaml"},
		{"patch", "deployment", "foo", "-p", `{"spec":{"replicas":0}}`},
		{"edit", "deploy", "foo"},
		{"scale", "deploy/foo", "--replicas=0"},
		{"rollout", "restart", "deploy/foo"},
		{"exec", "-it", "pod", "--", "sh"},
		{"attach", "pod"},
		{"cp", "src", "dst"},
		{"port-forward", "pod/foo", "8080:80"},
		{"proxy"},
		{"run", "nginx", "--image=nginx"},
		{"debug", "pod/foo"},
		{"wait", "--for=condition=ready", "pod/foo"},
		{"cordon", "node"},
		{"drain", "node"},
		{"taint", "node", "x=y:NoSchedule"},
		{"label", "pod", "foo", "bar=baz"},
		{"annotate", "pod", "foo", "k=v"},
		{"expose", "deploy/foo"},
		{"config", "use-context", "prod"},
		{"config", "set-context", "x"},
		{"auth", "reconcile"},
		{"logs", "p1", "-f"},
		{"logs", "p1", "--follow"},
	}
	for _, c := range cases {
		if err := Validate(c); err == nil {
			t.Errorf("Validate(%v) = nil, want error", c)
		}
	}
}

func TestValidate_RejectsForbiddenFlags(t *testing.T) {
	cases := [][]string{
		{"get", "pods", "--force"},
		{"get", "pods", "--grace-period=0"},
	}
	for _, c := range cases {
		err := Validate(c)
		if err == nil {
			t.Errorf("Validate(%v) = nil, want error", c)
			continue
		}
		if !strings.Contains(err.Error(), "forbidden token") {
			t.Errorf("Validate(%v) error %q, want 'forbidden token' mention", c, err)
		}
	}
}

func TestValidate_EmptyArgv(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Errorf("Validate(nil) = nil, want error")
	}
}
