package model

import "testing"

func TestClassifyPodHealth(t *testing.T) {
	cases := []struct {
		status, ready string
		want          PodHealth
	}{
		// Healthy
		{"Running", "1/1", HealthHealthy},
		{"Running", "3/3", HealthHealthy},
		{"Succeeded", "0/1", HealthHealthy},
		{"Completed", "0/1", HealthHealthy},
		{"", "", HealthHealthy},

		// Hard-bad: explicit failure states
		{"CrashLoopBackOff", "0/1", HealthHardBad},
		{"Error", "0/1", HealthHardBad},
		{"ImagePullBackOff", "0/1", HealthHardBad},
		{"OOMKilled", "0/1", HealthHardBad},
		{"Evicted", "0/1", HealthHardBad},
		{"Failed", "0/1", HealthHardBad},

		// Hard-bad: Running with not-all-ready (probe failure)
		{"Running", "0/1", HealthHardBad},
		{"Running", "1/2", HealthHardBad},
		{"Running", "0/0", HealthHardBad},

		// Soft-bad: transient
		{"Pending", "0/1", HealthSoftBad},
		{"Terminating", "0/1", HealthSoftBad},
		{"ContainerCreating", "0/1", HealthSoftBad},
		{"PodInitializing", "0/1", HealthSoftBad},
		{"Init:0/2", "0/1", HealthSoftBad},
		{"Init:1/3", "0/1", HealthSoftBad},
		{"Unknown", "", HealthSoftBad},

		// Hard-bad: exit codes / signals
		{"Sig:SIGKILL", "0/1", HealthHardBad},
		{"ExitCode:137", "0/1", HealthHardBad},
	}
	for _, c := range cases {
		got := ClassifyPodHealth(c.status, c.ready)
		if got != c.want {
			t.Errorf("ClassifyPodHealth(%q, %q) = %v, want %v", c.status, c.ready, got, c.want)
		}
	}
}
