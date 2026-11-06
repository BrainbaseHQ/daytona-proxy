package main

// ENG-1504 p0-guard: the PROXY_RESILIENCE switch and the budgets.md §2.7
// constants. Off — unset, empty, 0/false/off or garbage — is exactly
// today's behaviour; only 1/true/on (case-insensitive) read as on.

import (
	"os"
	"testing"
	"time"
)

func TestResilienceEnabledOnValues(t *testing.T) {
	for _, raw := range []string{"1", "true", "TRUE", "On", " on "} {
		if !resilienceEnabled(raw) {
			t.Errorf("resilienceEnabled(%q) = false, want true", raw)
		}
	}
}

func TestResilienceEnabledOffValues(t *testing.T) {
	for _, raw := range []string{"", "0", "false", "off", "OFF", "garbage", " "} {
		if resilienceEnabled(raw) {
			t.Errorf("resilienceEnabled(%q) = true, want false", raw)
		}
	}
}

func TestResilienceEnabledUnsetEnvironmentIsOff(t *testing.T) {
	t.Setenv("PROXY_RESILIENCE", "")
	if resilienceEnabled() {
		t.Error("unset PROXY_RESILIENCE read as on; want off")
	}
}

func TestResilienceEnabledReadsTheEnvironment(t *testing.T) {
	t.Setenv("PROXY_RESILIENCE", "1")
	if !resilienceEnabled() {
		t.Error("PROXY_RESILIENCE=1 read as off; want on")
	}
	t.Setenv("PROXY_RESILIENCE", "0")
	if resilienceEnabled() {
		t.Error("PROXY_RESILIENCE=0 read as on; want off")
	}
	if os.Getenv("PROXY_RESILIENCE") != "0" {
		t.Fatal("t.Setenv did not apply")
	}
}

func TestBudgetsConstantsMatchTheContract(t *testing.T) {
	if resolveRetries != 2 {
		t.Errorf("resolveRetries = %d, want 2", resolveRetries)
	}
	if staleIfErrorWindow != 60*time.Second {
		t.Errorf("staleIfErrorWindow = %v, want 60s", staleIfErrorWindow)
	}
	if negativeCacheTTL != 10*time.Second {
		t.Errorf("negativeCacheTTL = %v, want 10s", negativeCacheTTL)
	}
	if maxIdleConnsPerHost != 64 {
		t.Errorf("maxIdleConnsPerHost = %d, want 64", maxIdleConnsPerHost)
	}
}
