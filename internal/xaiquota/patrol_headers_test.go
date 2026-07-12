package xaiquota

import (
	"net/http"
	"testing"
)

func TestMergeProbeHeaders_defaultsWhenEmpty(t *testing.T) {
	h := mergeProbeHeaders(nil)
	if h["x-grok-client-version"] != DefaultProbeCLIVersion {
		t.Fatalf("version=%q want %q", h["x-grok-client-version"], DefaultProbeCLIVersion)
	}
	if h["x-xai-token-auth"] != "xai-grok-cli" {
		t.Fatalf("token-auth=%q", h["x-xai-token-auth"])
	}
	if h["User-Agent"] == "" {
		t.Fatal("User-Agent empty")
	}
}

func TestMergeProbeHeaders_fileOverrides(t *testing.T) {
	h := mergeProbeHeaders(map[string]string{
		"x-grok-client-version": "9.9.9",
		"X-Custom":              "yes",
	})
	if h["x-grok-client-version"] != "9.9.9" {
		t.Fatalf("override version=%q", h["x-grok-client-version"])
	}
	if h["X-Custom"] != "yes" {
		t.Fatalf("custom missing: %#v", h)
	}
	// defaults still present
	if h["x-xai-token-auth"] != "xai-grok-cli" {
		t.Fatalf("default token-auth lost")
	}
}

func TestIsCLIVersionRejected(t *testing.T) {
	body := `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later"}`
	if !isCLIVersionRejected(426, body) {
		t.Fatal("426+outdated should reject")
	}
	if !isCLIVersionRejected(http.StatusUpgradeRequired, "x") {
		t.Fatal("426 alone should reject")
	}
	if isCLIVersionRejected(403, `{"code":"permission-denied"}`) {
		t.Fatal("403 permission should not be CLI reject")
	}
}