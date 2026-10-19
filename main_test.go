package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRedactMasksCommonSecrets(t *testing.T) {
	in := strings.Join([]string{
		"AWS key AKIAABCDEFGHIJKLMNOP used",
		"Authorization: Bearer abcdef1234567890abcdef",
		"DATABASE_PASSWORD=hunter2hunter2",
		`api_key: "sk-live-1234567890"`,
		"connecting to postgres://app:s3cr3t@db.internal:5432/app",
		"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"user jane.doe@example.com logged in from 10.1.2.3",
	}, "\n")
	out, counts := Redact(in, false)
	for _, leaked := range []string{"AKIAABCDEFGHIJKLMNOP", "abcdef1234567890abcdef", "hunter2hunter2",
		"sk-live-1234567890", "s3cr3t", "eyJhbGciOiJIUzI1NiJ9", "jane.doe@example.com", "10.1.2.3"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"DATABASE_PASSWORD=[REDACTED-SECRET]", "postgres://[REDACTED-URL-CREDS]@db.internal"} {
		if !strings.Contains(out, kept) {
			t.Errorf("want %q in:\n%s", kept, out)
		}
	}
	if counts["IP"] != 1 {
		t.Errorf("IP count = %d, want 1", counts["IP"])
	}
}

func TestRedactKeepsCIDRAndPlaceholders(t *testing.T) {
	in := "- \"10.0.0.0/16\",\n+ \"0.0.0.0/0\",\nlisten 0.0.0.0:8080\nSTRIPE_API_KEY: <set to the key 'stripe-api-key' in secret 'payments-secrets'>"
	out, _ := Redact(in, false)
	for _, kept := range []string{"10.0.0.0/16", "0.0.0.0/0", "0.0.0.0:8080", "<set to the key"} {
		if !strings.Contains(out, kept) {
			t.Errorf("want %q kept in:\n%s", kept, out)
		}
	}
}

func TestRedactLeavesEnvReferencesAlone(t *testing.T) {
	line := `Env var STRIPE_API_KEY comes from Secret "payments-secrets" key "stripe-api-key"`
	if out, counts := Redact(line, false); out != line {
		t.Errorf("changed an env reference: %q %v", out, counts)
	}
}

func TestRedactLeavesNormalLinesAlone(t *testing.T) {
	line := "Back-off restarting failed container app in pod web-7d9f (exit code 1)"
	out, counts := Redact(line, false)
	if out != line || len(counts) != 0 {
		t.Errorf("changed a normal line: %q %v", out, counts)
	}
}

func TestTrimKeepsErrorsAndTail(t *testing.T) {
	lines := make([]string, 5000)
	for i := range lines {
		lines[i] = fmt.Sprintf("INFO request %d ok", i)
	}
	lines[1200] = "FATAL: could not connect to database"
	out := Trim(strings.Join(lines, "\n"), 4000)
	if len(out) > 4100 {
		t.Errorf("too long: %d", len(out))
	}
	if !strings.Contains(out, "FATAL: could not connect") || !strings.Contains(out, "INFO request 4999 ok") {
		t.Error("lost the error line or the tail")
	}
}

func pod(t *testing.T, js string) Pod {
	t.Helper()
	var p Pod
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetect(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, js, want string
	}{
		{"oom beats crashloop", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Running","containerStatuses":[{"name":"c","restartCount":3,
			"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-10-18T11:59:00Z"}}}]}}`, "OOMKilled"},
		{"image pull", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Pending","containerStatuses":[{"name":"c","state":{"waiting":{"reason":"ErrImagePull"}}}]}}`, "ImagePullBackOff"},
		{"missing secret", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Pending","containerStatuses":[{"name":"c","state":{"waiting":{"reason":"CreateContainerConfigError","message":"secret \"x\" not found"}}}]}}`, "CreateContainerConfigError"},
		{"running between crashes", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Running","containerStatuses":[{"name":"c","restartCount":4,"ready":true,
			"state":{"running":{"startedAt":"2026-10-18T11:59:58Z"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"finishedAt":"2026-10-18T11:59:50Z"}}}]}}`, "CrashLoopBackOff"},
		{"unschedulable", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes"}]}}`, "Unschedulable"},
		{"healthy", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":true,"state":{"running":{"startedAt":"2026-10-18T10:00:00Z"}}}]}}`, ""},
		{"old crash is history", `{"metadata":{"name":"w","namespace":"d"},"status":{"phase":"Running","containerStatuses":[{"name":"c","restartCount":1,"ready":true,
			"state":{"running":{"startedAt":"2026-10-18T10:00:00Z"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"finishedAt":"2026-10-18T09:59:00Z"}}}]}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect(pod(t, c.js), now)
			reason := ""
			if len(got) > 0 {
				reason = got[0].Reason
			}
			if reason != c.want {
				t.Errorf("got %q, want %q (%+v)", reason, c.want, got)
			}
		})
	}
}
