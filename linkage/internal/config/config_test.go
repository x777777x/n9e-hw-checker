package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRejectsEmptyToken(t *testing.T) {
	p := writeTemp(t, `{"server":{"addr":":8080","token":""},"es":{"addr":"http://x","index":"syslog-system"},"llm":{"base_url":"http://x","model":"m"}}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("want error on empty token")
	}
}

func TestLoadRejectsMissingLLM(t *testing.T) {
	p := writeTemp(t, `{"server":{"token":"t"},"es":{"addr":"http://x","index":"syslog-system"},"llm":{"base_url":"http://x"}}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("want error when llm.model missing and agent disabled")
	}
}

func TestLoadClampsZeroWindow(t *testing.T) {
	p := writeTemp(t, `{"server":{"token":"t"},"es":{"addr":"http://x","index":"syslog-system"},"llm":{"base_url":"http://x","model":"m"},"window":{"dedup_minutes":0,"merge_seconds":0}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Window.DedupMinutes <= 0 || c.Window.MergeSeconds <= 0 {
		t.Fatalf("window should be clamped, got %+v", c.Window)
	}
}

func TestLoadAgentEnabledRequiresExecutor(t *testing.T) {
	p := writeTemp(t, `{"server":{"token":"t"},"agent":{"enabled":true}}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("want error when agent enabled without executor_url")
	}
}

func TestLoadClampsTimeouts(t *testing.T) {
	p := writeTemp(t, `{"server":{"token":"t"},"es":{"addr":"http://x","index":"syslog-system"},"llm":{"base_url":"http://x","model":"m","timeout_s":0},"agent":{"timeout_s":0}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.TimeoutS <= 0 {
		t.Fatalf("llm timeout should be clamped, got %d", c.LLM.TimeoutS)
	}
	if c.Agent.TimeoutS <= 0 {
		t.Fatalf("agent timeout should be clamped, got %d", c.Agent.TimeoutS)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	p := writeTemp(t, `{"server":{"token":"t"},"es":{"addr":"http://x","index":"syslog-system"},"llm":{"base_url":"http://x","model":"m"},"typo_field":1}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("want error on unknown config field")
	}
}
