package xaiquota

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyNetworkProbe(t *testing.T) {
	cases := []struct {
		err  string
		code int
		sub  string
	}{
		{"Get \"https://x\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", -1, "timeout"},
		{"Post \"https://x\": context canceled", -2, "canceled"},
		{"dial tcp: lookup cli-chat-proxy.grok.com: no such host", -3, "dns"},
		{"tls: failed to verify certificate: x509: certificate has expired", -4, "tls"},
		{"dial tcp 1.2.3.4:443: connect: connection refused", -5, "connect"},
		{"something weird happened", 0, "network"},
	}
	for _, c := range cases {
		code, reason := classifyNetworkProbe(errors.New(c.err))
		if code != c.code {
			t.Fatalf("err=%q code=%d want %d reason=%s", c.err, code, c.code, reason)
		}
		if !strings.Contains(strings.ToLower(reason), c.sub) {
			t.Fatalf("reason=%q want sub %q", reason, c.sub)
		}
		act := probeErrorKind(code, reason)
		if act == "alive" || act == "deleted" {
			t.Fatalf("bad action %s", act)
		}
	}
}

func TestProbeErrorKindHTTP(t *testing.T) {
	if probeErrorKind(426, "CLI version") != "cli_version" {
		t.Fatal("426")
	}
	if probeErrorKind(422, "x") != "probe_unprocessable" {
		t.Fatal("422")
	}
	if probeErrorKind(503, "x") != "probe_http_5xx" {
		t.Fatal("503")
	}
	if probeErrorKind(404, "x") != "probe_http_4xx" {
		t.Fatal("404")
	}
	if probeErrorKind(403, "not available in your region") != "region_block" {
		t.Fatal("region")
	}
}
