package xaiquota

import (
	"strings"
	"testing"
)

func TestIsTransportNetFail(t *testing.T) {
	cases := []struct {
		act  string
		code int
		want bool
	}{
		{"net_timeout", -1, true},
		{"net_dns", -3, true},
		{"net_tls", -4, true},
		{"net_connect", -5, true},
		{"net_error", 0, true},
		{"net_canceled", -2, false},
		{"alive", 200, false},
		{"deleted", 403, false},
		{"cooldown", 429, false},
		{"region_block", 403, false},
		{"probe_http_5xx", 503, false},
		{"patrol_abort_net", 0, false},
		{"", -1, true},
		{"unknown", 0, false},
	}
	for _, c := range cases {
		got := isTransportNetFail(c.act, c.code)
		if got != c.want {
			t.Fatalf("act=%s code=%d got=%v want=%v", c.act, c.code, got, c.want)
		}
	}
}

func TestRunConnectivityGateMessage(t *testing.T) {
	// Direct internet check may succeed or fail depending on environment;
	// ensure report fields are populated and message non-empty.
	rep := runConnectivityGate("")
	if rep.Message == "" {
		t.Fatal("empty message")
	}
	if rep.ProxyConfigured {
		t.Fatal("empty proxy should not be configured")
	}
	if !rep.ProxyOK {
		t.Fatal("empty proxy should be N/A ok")
	}
	// Abort only when internet fails (no proxy to fail).
	if !rep.InternetOK && !rep.Abort {
		t.Fatal("internet down must abort")
	}
	if rep.InternetOK && rep.Abort {
		t.Fatal("healthy internet + no proxy must not abort")
	}
}

func TestRunConnectivityGateBadProxyURL(t *testing.T) {
	rep := runConnectivityGate("://bad")
	if !rep.ProxyConfigured {
		t.Fatal("bad url still configured")
	}
	if rep.ProxyOK {
		t.Fatal("bad proxy url must fail")
	}
	if !rep.Abort {
		t.Fatal("bad proxy must abort")
	}
	if !strings.Contains(rep.Message, "代理") && !strings.Contains(rep.Message, "proxy") {
		// Chinese message
		if !strings.Contains(rep.Message, "中止") {
			t.Fatalf("msg=%s", rep.Message)
		}
	}
}

func TestConnectivityAbortConstants(t *testing.T) {
	if consecutiveNetFailThreshold < 3 {
		t.Fatal("threshold too low")
	}
	if connectivityCheckTimeout <= 0 {
		t.Fatal("timeout")
	}
	if len(connectivityProbeURLs) < 2 {
		t.Fatal("need probe urls")
	}
}
