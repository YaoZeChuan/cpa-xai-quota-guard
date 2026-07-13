package xaiquota

import (
	"strconv"
	"runtime"
	"fmt"
	"io"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPatrolModel is the default probe model for patrol.
// Paid models (e.g. grok-3 full) often return personal-team-blocked:spending-limit
// even when free-tier chat still works — causing false dead/cooldown signals.
const DefaultPatrolModel = "grok-4.5"

// DefaultProbeBaseURL matches Grok CLI chat-proxy used by CPA oauth auth files.
const DefaultProbeBaseURL = "https://cli-chat-proxy.grok.com/v1"

// DefaultProbeCLIVersion is the minimum Grok CLI client version accepted by
// cli-chat-proxy (HTTP 426 when missing/outdated: "CLI version (none) is outdated").
// Value taken from CPA oauth auth-file headers (x-grok-client-version).
const DefaultProbeCLIVersion = "0.2.93"

// SuggestedPatrolModels are common xAI ids shown when live /models is empty.
var SuggestedPatrolModels = []string{
	"grok-4.5",
	"grok-4.5-build-free",
	"grok-3-mini",
	"grok-3",
	"grok-2-1212",
	"grok-2",
}

// patrolState tracks the in-progress or last-completed sweep.
type patrolState struct {
	mu            sync.Mutex
	running       bool
	startedAtMS   int64
	completedAtMS int64
	totalCandidates int
	totalProbed   int
	totalDeleted  int
	totalErrors   int
	totalAlive    int
	totalSkipped  int
	totalCooldown     int
	total429CD   int // free-usage 429 cooldown
	totalSpendCD  int // 402 spending cooldown
	totalReenabled int
	byHTTP        map[int]int
	byAction      map[string]int
	workers       int // current elastic target
	workersMax    int // user hard cap
	workersMin    int
	load1         float64
	scaleReason   string
	lastError     string
	lastSweepLog  []patrolLogEntry
	scope         string
	stopRequested bool
	lastPersistMS int64
	// sliding window for elastic scaling (timeout/error pressure)
	winTotal    int
	winTimeout  int
	winNetErr   int
}

// patrolLogEntry is an alias of durable PatrolLogEntry.
type patrolLogEntry = PatrolLogEntry


// PatrolStatus is the JSON view for the UI.
type PatrolStatus struct {
	Running         bool             `json:"running"`
	StartedAtMS     int64            `json:"started_at_ms"`
	CompletedAtMS   int64            `json:"completed_at_ms"`
	TotalCandidates int              `json:"total_candidates"`
	TotalProbed     int              `json:"total_probed"`
	TotalDeleted    int              `json:"total_deleted"`
	TotalErrors     int              `json:"total_errors"`
	TotalAlive      int              `json:"total_alive"`
	TotalSkipped    int              `json:"total_skipped"`
	TotalCooldown       int              `json:"total_cooldown"`
	Total429CD      int              `json:"total_429_cooldown"`
	TotalSpendCD    int              `json:"total_402_cooldown"`
	TotalReenabled  int              `json:"total_reenabled"`
	ByHTTP          map[string]int   `json:"by_http,omitempty"`
	ByAction        map[string]int   `json:"by_action,omitempty"`
	Workers         int              `json:"workers"`
	WorkersMax      int              `json:"workers_max,omitempty"`
	WorkersMin      int              `json:"workers_min,omitempty"`
	Load1           float64          `json:"load1,omitempty"`
	ScaleReason     string           `json:"scale_reason,omitempty"`
	LastError       string           `json:"last_error,omitempty"`
	Scope           string           `json:"scope,omitempty"`
	SavedAtMS       int64            `json:"saved_at_ms,omitempty"`
	RecentLog       []patrolLogEntry `json:"recent_log,omitempty"`
}

// authFileJSON is the on-disk structure of a CPA auth file.
type authFileJSON struct {
	AccessToken string            `json:"access_token"`
	BaseURL     string            `json:"base_url"`
	AuthKind    string            `json:"auth_kind"`
	Type        string            `json:"type"`
	Email       string            `json:"email"`
	Disabled    bool              `json:"disabled"`
	Headers     map[string]string `json:"headers"`
}

// defaultProbeHeaders returns Grok CLI identity headers required by cli-chat-proxy.
// Without these, upstream returns HTTP 426 "Your Grok CLI version (none) is outdated".
func defaultProbeHeaders() map[string]string {
	return map[string]string{
		"User-Agent":               "grok-pager/" + DefaultProbeCLIVersion + " grok-shell/" + DefaultProbeCLIVersion + " (linux; x86_64)",
		"x-authenticateresponse":   "authenticate-response",
		"x-grok-client-identifier": "grok-pager",
		"x-grok-client-version":    DefaultProbeCLIVersion,
		"x-xai-token-auth":         "xai-grok-cli",
	}
}

// mergeProbeHeaders starts from default Grok CLI headers, then overlays auth-file headers
// (file values win). Empty keys/values are skipped.
func mergeProbeHeaders(fileHeaders map[string]string) map[string]string {
	out := defaultProbeHeaders()
	for k, v := range fileHeaders {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		found := ""
		for ek := range out {
			if strings.EqualFold(ek, k) {
				found = ek
				break
			}
		}
		if found != "" {
			delete(out, found)
		}
		out[k] = v
	}
	return out
}

// isCLIVersionRejected reports cli-chat-proxy HTTP 426 / outdated CLI client identity.
// This is a probe-client issue, NOT a dead credential — never delete on this signal.
func isCLIVersionRejected(statusCode int, body string) bool {
	if statusCode == http.StatusUpgradeRequired { // 426
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "cli version") && strings.Contains(lower, "outdated")
}


// probeResult holds the outcome of probing one credential.
type probeResult struct {
	authIndex string
	fileName  string
	account   string
	action    string // alive|deleted|error|cooldown|cooldown_skip|reenabled|net_*|probe_*|region_block|cli_version
	reason    string
	httpCode  int
	modelUsed string
}

// classifyNetworkProbe maps transport failures into synthetic http buckets for stats/UI:
//   0  = generic network
//  -1  = timeout / deadline
//  -2  = context canceled / client abort
//  -3  = DNS / resolve failure
//  -4  = TLS / certificate
//  -5  = connection refused / dial
func classifyNetworkProbe(err error) (httpCode int, reason string) {
	if err == nil {
		return 0, "network error"
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded") || strings.Contains(low, "i/o timeout") || strings.Contains(low, "client.timeout"):
		return -1, "timeout: " + msg
	case strings.Contains(low, "context canceled") || strings.Contains(low, "context cancelled") || strings.Contains(low, "request canceled"):
		return -2, "canceled: " + msg
	case strings.Contains(low, "no such host") || strings.Contains(low, "server misbehaving") || strings.Contains(low, "dns") || strings.Contains(low, "lookup "):
		return -3, "dns: " + msg
	case strings.Contains(low, "tls") || strings.Contains(low, "x509") || strings.Contains(low, "certificate") || strings.Contains(low, "handshake"):
		return -4, "tls: " + msg
	case strings.Contains(low, "connection refused") || strings.Contains(low, "connectex") || strings.Contains(low, "dial tcp") || strings.Contains(low, "network is unreachable") || strings.Contains(low, "no route to host") || strings.Contains(low, "connection reset"):
		return -5, "connect: " + msg
	default:
		return 0, "network: " + msg
	}
}

// probeErrorKind returns a stable action key for non-fatal probe failures (UI by_action).
func probeErrorKind(httpCode int, reason string) string {
	low := strings.ToLower(reason)
	switch httpCode {
	case -1:
		return "net_timeout"
	case -2:
		return "net_canceled"
	case -3:
		return "net_dns"
	case -4:
		return "net_tls"
	case -5:
		return "net_connect"
	case 0:
		return "net_error"
	case 404, 405:
		return "probe_http_4xx"
	case 408, 409, 418, 421, 423, 424, 425, 428, 431, 451:
		return "probe_http_4xx"
	case 422:
		return "probe_unprocessable"
	case 426:
		return "cli_version"
	case 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526, 527, 530:
		return "probe_http_5xx"
	}
	if strings.Contains(low, "区域") || strings.Contains(low, "region") || strings.Contains(low, "not available in your region") {
		return "region_block"
	}
	if strings.Contains(low, "cli版本") || strings.Contains(low, "cli version") {
		return "cli_version"
	}
	if httpCode >= 500 {
		return "probe_http_5xx"
	}
	if httpCode >= 400 {
		return "probe_http_4xx"
	}
	return "error"
}


// PatrolOptions controls one sweep.
type PatrolOptions struct {
	// Scope: ""/"all" = enabled xAI only;
	// "spending_only" = plugin_auto disabled cooldowns only (no enabled accounts).
	Scope string `json:"scope"`
}


// clampPatrolUserMax normalizes the user-configured hard upper bound.
func clampPatrolUserMax(userMax int) int {
	if userMax <= 0 {
		return 16
	}
	if userMax > 64 {
		return 64
	}
	return userMax
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readLoadAvg1 returns 1-minute load average when /proc/loadavg is available (Linux).
func readLoadAvg1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// resolvePatrolWorkers is the initial target: aggressive network-bound start, still <= user max.
// Elastic controller will climb to userMax when load/probe health allow.
func resolvePatrolWorkers(userMax, candidates int) int {
	maxW := clampPatrolUserMax(userMax)
	ncpu := runtime.NumCPU()
	if ncpu < 1 {
		ncpu = 1
	}
	// I/O-bound probes: start at max(4×CPU, half of user cap) so sweeps do not crawl at 2–3 workers.
	auto := ncpu * 4
	half := maxW / 2
	if half < ncpu*2 {
		half = ncpu * 2
	}
	if auto < half {
		auto = half
	}
	if auto < 4 && maxW >= 4 {
		auto = 4
	}
	if auto < 2 && maxW >= 2 {
		auto = 2
	}
	if auto > maxW {
		auto = maxW
	}
	if candidates > 0 && auto > candidates {
		auto = candidates
	}
	if auto < 1 {
		auto = 1
	}
	return auto
}

// elasticPatrolTarget picks concurrency inside [min,max] from live load + recent probe health.
// Goal: finish the sweep ASAP without wedging the CPA host or drowning the proxy.
func elasticPatrolTarget(userMax, candidates, cur int, load1 float64, loadOK bool, timeoutRate, netErrRate float64) (target int, reason string) {
	maxW := clampPatrolUserMax(userMax)
	if candidates > 0 && maxW > candidates {
		maxW = candidates
	}
	ncpu := runtime.NumCPU()
	if ncpu < 1 {
		ncpu = 1
	}
	minW := ncpu
	if minW < 2 {
		minW = 2
	}
	if minW > maxW {
		minW = maxW
	}
	if candidates > 0 && minW > candidates {
		minW = candidates
	}
	if minW < 1 {
		minW = 1
	}

	base := ncpu * 3
	reason = "base_3x_cpu"
	if loadOK {
		lpc := load1 / float64(ncpu)
		switch {
		case lpc >= 1.5:
			base = ncpu / 2
			if base < 1 {
				base = 1
			}
			reason = "load_critical"
		case lpc >= 1.0:
			base = ncpu
			reason = "load_high"
		case lpc >= 0.7:
			base = ncpu * 2
			reason = "load_moderate"
		case lpc >= 0.4:
			base = ncpu * 4
			if base < maxW/2 {
				base = maxW / 2
			}
			reason = "load_ok"
		default:
			// Host mostly idle: aim at user hard cap immediately.
			base = maxW
			reason = "load_idle_max"
		}
	} else {
		base = ncpu * 3
		reason = "no_loadavg_3x"
	}

	if timeoutRate >= 0.35 || netErrRate >= 0.45 {
		base = minW
		reason = "probe_pressure_min"
	} else if timeoutRate >= 0.18 || netErrRate >= 0.25 {
		base = minW
		if ncpu > minW {
			base = ncpu
		}
		if base < minW {
			base = minW
		}
		reason = "probe_pressure_half"
	} else if timeoutRate < 0.08 && netErrRate < 0.12 && (!loadOK || load1/float64(ncpu) < 0.75) {
		// Healthy probes: jump toward user max quickly (network I/O bound, not CPU bound).
		if base < maxW {
			step := (maxW - base + 1) * 2 / 3
			if step < ncpu {
				step = ncpu
			}
			if step < 2 {
				step = 2
			}
			base = base + step
		}
		if base > maxW {
			base = maxW
		}
		if base >= maxW {
			reason = reason + "+max"
		} else {
			reason = reason + "+climb"
		}
	}

	if base > maxW {
		base = maxW
	}
	if base < minW {
		base = minW
	}
	if candidates > 0 && base > candidates {
		base = candidates
	}

	if cur > 0 {
		// Allow aggressive ramp-up when climbing toward max.
		upStep := cur
		if upStep < ncpu*2 {
			upStep = ncpu * 2
		}
		if upStep < 4 {
			upStep = 4
		}
		up := cur + upStep
		// Under critical load / probe pressure, shrink hard (no soft floor).
		hardDown := strings.Contains(reason, "load_critical") || strings.Contains(reason, "probe_pressure")
		downStep := cur / 2
		if hardDown {
			downStep = cur * 3 / 4
		}
		if downStep < 1 {
			downStep = 1
		}
		down := cur - downStep
		if hardDown {
			// allow dropping straight to computed base/min
			down = base
		}
		if down < minW {
			down = minW
		}
		if base > up {
			base = up
			reason += "/smooth_up"
		}
		if base < down {
			base = down
			reason += "/smooth_down"
		}
	}
	if base < 1 {
		base = 1
	}
	return base, reason
}

// PatrolSweep iterates auth files with a worker pool, probes upstream, and acts on results.
func (g *Guard) PatrolSweep(opts PatrolOptions) PatrolStatus {
	g.patrol.mu.Lock()
	if g.patrol.running {
		g.patrol.mu.Unlock()
		return g.PatrolStatus()
	}
	g.patrol.running = true
	g.patrol.startedAtMS = time.Now().UnixMilli()
	g.patrol.completedAtMS = 0
	g.patrol.totalCandidates = 0
	g.patrol.totalProbed = 0
	g.patrol.totalDeleted = 0
	g.patrol.totalErrors = 0
	g.patrol.totalAlive = 0
	g.patrol.totalSkipped = 0
	g.patrol.totalCooldown = 0
	g.patrol.total429CD = 0
	g.patrol.totalSpendCD = 0
	g.patrol.totalReenabled = 0
	g.patrol.byHTTP = map[int]int{}
	g.patrol.byAction = map[string]int{}
	g.patrol.workers = 0
	g.patrol.lastError = ""
	g.patrol.lastSweepLog = nil
	g.patrol.scope = ""
	g.patrol.lastPersistMS = 0
	g.patrol.stopRequested = false
	g.patrol.mu.Unlock()

	defer func() {
		g.patrol.mu.Lock()
		g.patrol.running = false
		g.patrol.completedAtMS = time.Now().UnixMilli()
		g.patrol.mu.Unlock()
		g.persistPatrolSnapshot(true)
	}()

	cfg := g.Config()
	if !cfg.Enabled {
		g.setPatrolError("plugin disabled")
		return g.PatrolStatus()
	}
	if g.auth == nil {
		g.setPatrolError("auth lookup nil")
		return g.PatrolStatus()
	}
	authDir := strings.TrimSpace(cfg.PatrolAuthDir)
	if authDir == "" {
		g.setPatrolError("patrol_auth_dir not configured")
		return g.PatrolStatus()
	}

	files, err := g.auth.List()
	if err != nil {
		g.setPatrolError(fmt.Sprintf("list auth files: %v", err))
		return g.PatrolStatus()
	}

	scope := strings.ToLower(strings.TrimSpace(opts.Scope))
	if scope == "" {
		scope = "all"
	}
	// all: ONLY currently-enabled xAI credentials (skip any disabled, including cooldowns)
	// spending_only / cooldown recheck: ONLY plugin_auto owned disabled cooldowns
	//   (429 free-usage + 402 spending_limit); never probe still-enabled accounts
	candidates := make([]AuthFile, 0, len(files))
	for _, f := range files {
		if !IsXAIProvider(f.Provider, "") {
			continue
		}
		live := g.storeGet(f.AuthIndex)
		isPluginCool := live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Owner == Owner && !live.PreDisabled &&
			(live.Signal == "spending_limit" || live.Signal == "body.error.code=subscription:free-usage-exhausted" ||
				strings.Contains(live.Signal, "free-usage") || strings.Contains(live.Signal, "short_window") ||
				strings.HasPrefix(live.Signal, "body.error.code=subscription:"))
		// Prefer explicit signal families; also accept any plugin_auto auto-disabled with recover_at (generic cooldown)
		if !isPluginCool && live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Owner == Owner && !live.PreDisabled {
			isPluginCool = true
		}
		switch scope {
		case "spending_only":
			// button: 仅复核冷却号 — disabled cool-down accounts only
			if f.Disabled && isPluginCool {
				candidates = append(candidates, f)
			}
		default: // all / full patrol
			// button: 启动/全量巡查 — enabled only; never touch disabled
			if !f.Disabled {
				candidates = append(candidates, f)
			}
		}
	}
	g.patrol.mu.Lock()
	g.patrol.scope = scope
	g.patrol.mu.Unlock()
	g.logf("info", "patrol scope=%s candidates=%d auto_model_switch=%v model=%s", scope, len(candidates), cfg.PatrolAutoModelSwitch, cfg.PatrolModel)

	batchLimit := cfg.PatrolBatchSize
	if batchLimit > 0 && batchLimit < len(candidates) {
		candidates = candidates[:batchLimit]
	}

	userMax := clampPatrolUserMax(cfg.PatrolConcurrency)
	initial := resolvePatrolWorkers(userMax, len(candidates))
	load1, loadOK := readLoadAvg1()
	ncpu := runtime.NumCPU()
	minW := ncpu
	if minW < 2 {
		minW = 2
	}
	if minW > userMax {
		minW = userMax
	}
	if len(candidates) > 0 && minW > len(candidates) {
		minW = len(candidates)
	}
	if minW < 1 {
		minW = 1
	}

	g.patrol.mu.Lock()
	g.patrol.totalCandidates = len(candidates)
	g.patrol.workers = initial
	g.patrol.workersMax = userMax
	g.patrol.workersMin = minW
	g.patrol.load1 = load1
	g.patrol.scaleReason = "initial"
	g.patrol.winTotal = 0
	g.patrol.winTimeout = 0
	g.patrol.winNetErr = 0
	g.patrol.mu.Unlock()
	g.logf("info", "patrol elastic start workers=%d max=%d min=%d cpu=%d load1=%.2f load_ok=%v candidates=%d",
		initial, userMax, minW, ncpu, load1, loadOK, len(candidates))

	if len(candidates) == 0 {
		return g.PatrolStatus()
	}

	probeTimeout := time.Duration(cfg.PatrolTimeout) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}

	client := g.newPatrolHTTPClient(probeTimeout, cfg.PatrolProxyURL, userMax)

	// Elastic permits: spawn up to userMax cheap goroutines; concurrency = #permits in channel.
	maxW := userMax
	if maxW > len(candidates) {
		maxW = len(candidates)
	}
	if maxW < 1 {
		maxW = 1
	}
	jobs := make(chan AuthFile, maxW*4)
	permits := make(chan struct{}, maxW)
	for i := 0; i < initial && i < maxW; i++ {
		permits <- struct{}{}
	}
	var permitCur int32 = int32(initial)
	if int(permitCur) > maxW {
		permitCur = int32(maxW)
	}

	var wg sync.WaitGroup
	var stopFlag int32

	// Stop watcher.
	go func() {
		for {
			g.patrol.mu.Lock()
			stop := g.patrol.stopRequested
			running := g.patrol.running
			g.patrol.mu.Unlock()
			if stop || !running {
				atomic.StoreInt32(&stopFlag, 1)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	// Elastic controller: re-target concurrency from load + recent probe health.
	var controllerDone int32
	go func() {
		ticker := time.NewTicker(1000 * time.Millisecond)
		defer ticker.Stop()
		for {
			if atomic.LoadInt32(&controllerDone) == 1 || atomic.LoadInt32(&stopFlag) == 1 {
				return
			}
			<-ticker.C
			if atomic.LoadInt32(&stopFlag) == 1 {
				return
			}
			g.patrol.mu.Lock()
			tot := g.patrol.winTotal
			to := g.patrol.winTimeout
			ne := g.patrol.winNetErr
			// decay window so old pressure fades
			g.patrol.winTotal = tot / 2
			g.patrol.winTimeout = to / 2
			g.patrol.winNetErr = ne / 2
			cur := g.patrol.workers
			g.patrol.mu.Unlock()

			var timeoutRate, netErrRate float64
			if tot > 0 {
				timeoutRate = float64(to) / float64(tot)
				netErrRate = float64(ne) / float64(tot)
			}
			l1, ok := readLoadAvg1()
			desired, reason := elasticPatrolTarget(userMax, len(candidates), cur, l1, ok, timeoutRate, netErrRate)
			if desired > maxW {
				desired = maxW
			}
			if desired < 1 {
				desired = 1
			}

			// Grow / shrink permit tokens.
			for {
				c := int(atomic.LoadInt32(&permitCur))
				if c == desired {
					break
				}
				if c < desired {
					select {
					case permits <- struct{}{}:
						atomic.AddInt32(&permitCur, 1)
					default:
						// should not happen with sized channel
						atomic.StoreInt32(&permitCur, int32(desired))
						c = desired
					}
				} else {
					select {
					case <-permits:
						atomic.AddInt32(&permitCur, -1)
					case <-time.After(30 * time.Millisecond):
						// workers hold permits; try again next tick
						goto publish
					}
				}
			}
		publish:
			g.patrol.mu.Lock()
			g.patrol.workers = int(atomic.LoadInt32(&permitCur))
			g.patrol.workersMax = userMax
			g.patrol.workersMin = minW
			g.patrol.load1 = l1
			g.patrol.scaleReason = reason
			g.patrol.mu.Unlock()
			g.logf("info", "patrol elastic target=%d cur=%d max=%d load1=%.2f to_rate=%.2f net_rate=%.2f reason=%s",
				desired, atomic.LoadInt32(&permitCur), userMax, l1, timeoutRate, netErrRate, reason)
		}
	}()

	for i := 0; i < maxW; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if atomic.LoadInt32(&stopFlag) == 1 {
					return
				}
				// acquire elastic permit (may block when scaled down)
				select {
				case <-permits:
				default:
					// wait until permit or stop
					for {
						if atomic.LoadInt32(&stopFlag) == 1 {
							return
						}
						select {
						case <-permits:
							goto got
						case <-time.After(50 * time.Millisecond):
						}
					}
				}
			got:
				result := g.probeOneCredential(f, authDir, client)
				g.recordProbeResult(result)
				// release permit (keep pool size = permitCur)
				select {
				case permits <- struct{}{}:
				default:
					// if controller shrank capacity, drop token
				}
			}
		}()
	}

	for _, f := range candidates {
		if atomic.LoadInt32(&stopFlag) == 1 {
			break
		}
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	atomic.StoreInt32(&controllerDone, 1)

	return g.PatrolStatus()
}

func (g *Guard) setPatrolError(msg string) {
	g.patrol.mu.Lock()
	g.patrol.lastError = msg
	g.patrol.mu.Unlock()
}

// patrolHTTP holds a reusable client so SOCKS/proxy sessions and TCP connections
// are reused across probes (avoid rotating egress IP on every request).
type patrolHTTP struct {
	mu       sync.Mutex
	client   *http.Client
	proxyURL string
	timeout  time.Duration
}

func (g *Guard) newPatrolHTTPClient(timeout time.Duration, proxyURL string, maxConns ...int) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	mc := 64
	if len(maxConns) > 0 && maxConns[0] > 0 {
		mc = maxConns[0]
		if mc < 16 {
			mc = 16
		}
		if mc > 128 {
			mc = 128
		}
	}
	// Reuse shared client when proxy+timeout match.
	g.patrolHTTP.mu.Lock()
	defer g.patrolHTTP.mu.Unlock()
	if g.patrolHTTP.client != nil && g.patrolHTTP.proxyURL == proxyURL && g.patrolHTTP.timeout == timeout {
		return g.patrolHTTP.client
	}
	tr := &http.Transport{
		MaxIdleConns:          maxInt(128, mc*2),
		MaxIdleConnsPerHost:   mc,
		MaxConnsPerHost:       mc,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   12 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	c := &http.Client{Timeout: timeout, Transport: tr}
	g.patrolHTTP.client = c
	g.patrolHTTP.proxyURL = proxyURL
	g.patrolHTTP.timeout = timeout
	return c
}

// InvalidatePatrolHTTP drops the shared client (e.g. after proxy config change).
func (g *Guard) InvalidatePatrolHTTP() {
	g.patrolHTTP.mu.Lock()
	defer g.patrolHTTP.mu.Unlock()
	if g.patrolHTTP.client != nil {
		if tr, ok := g.patrolHTTP.client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	g.patrolHTTP.client = nil
	g.patrolHTTP.proxyURL = ""
	g.patrolHTTP.timeout = 0
}

func (g *Guard) recordProbeResult(r probeResult) {
	g.patrol.mu.Lock()
	g.patrol.totalProbed++
	g.patrol.winTotal++
	if r.httpCode == -1 {
		g.patrol.winTimeout++
	}
	if r.httpCode <= -2 || r.httpCode == 0 {
		// synthetic net buckets / unknown transport
		if r.httpCode != -1 {
			g.patrol.winNetErr++
		}
	}
	if g.patrol.byHTTP == nil {
		g.patrol.byHTTP = map[int]int{}
	}
	if g.patrol.byAction == nil {
		g.patrol.byAction = map[string]int{}
	}
	g.patrol.byHTTP[r.httpCode]++
	act := r.action
	if act == "" {
		act = "unknown"
	}
	g.patrol.byAction[act]++
	switch act {
	case "deleted":
		g.patrol.totalDeleted++
	case "error", "net_timeout", "net_canceled", "net_dns", "net_tls", "net_connect", "net_error",
		"probe_http_4xx", "probe_http_5xx", "probe_unprocessable", "region_block", "cli_version":
		g.patrol.totalErrors++
	case "cooldown_skip":
		g.patrol.totalSkipped++
	case "cooldown":
		g.patrol.totalCooldown++
		if r.httpCode == http.StatusTooManyRequests {
			g.patrol.total429CD++
		} else if r.httpCode == http.StatusPaymentRequired {
			g.patrol.totalSpendCD++
		}
	case "reenabled":
		g.patrol.totalReenabled++
		g.patrol.totalAlive++
	case "alive":
		g.patrol.totalAlive++
	default:
		// unknown action: count as error, never inflate alive
		g.patrol.totalErrors++
		if act == "unknown" {
			g.patrol.byAction["error"]++
		}
	}
	entry := patrolLogEntry{
		TimeMS:    time.Now().UnixMilli(),
		AuthIndex: r.authIndex,
		FileName:  r.fileName,
		Account:   r.account,
		Action:    act,
		Reason:    r.reason,
		HTTPCode:  r.httpCode,
	}
	if len(g.patrol.lastSweepLog) >= 500 {
		g.patrol.lastSweepLog = g.patrol.lastSweepLog[len(g.patrol.lastSweepLog)-499:]
	}
	g.patrol.lastSweepLog = append(g.patrol.lastSweepLog, entry)
	probed := g.patrol.totalProbed
	lastP := g.patrol.lastPersistMS
	g.patrol.mu.Unlock()

	// Persist material actions into durable action_history (survives restart).
	switch entry.Action {
	case "deleted", "cooldown", "reenabled", "error",
		"net_timeout", "net_canceled", "net_dns", "net_tls", "net_connect", "net_error",
		"probe_http_4xx", "probe_http_5xx", "probe_unprocessable", "region_block", "cli_version":
		if g.store != nil {
			_ = g.store.AppendAction(ActionEvent{
				TimeMS: entry.TimeMS, Action: entry.Action, Source: "patrol",
				AuthIndex: r.authIndex, FileName: r.fileName, Account: r.account,
				HTTPCode: r.httpCode, Reason: r.reason,
			})
		}
	}
	// Checkpoint snapshot every 25 probes so mid-sweep crash still keeps progress.
	if probed > 0 && probed%25 == 0 {
		now := time.Now().UnixMilli()
		if now-lastP > 3000 {
			g.persistPatrolSnapshot(false)
		}
	}
}

// probeOneCredential reads the auth file, extracts token, sends a minimal probe.
func (g *Guard) probeOneCredential(f AuthFile, authDir string, client *http.Client) probeResult {
	filePath := filepath.Join(authDir, f.Name)
	raw, err := os.ReadFile(filePath)
	if err != nil {
		if !strings.HasSuffix(f.Name, ".json") {
			raw, err = os.ReadFile(filePath + ".json")
		}
	}
	if err != nil {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "error",
			reason:    fmt.Sprintf("read auth file: %v", err),
		}
	}

	var af authFileJSON
	if err := json.Unmarshal(raw, &af); err != nil {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "error",
			reason:    fmt.Sprintf("parse auth file: %v", err),
		}
	}
	if af.AccessToken == "" {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "error",
			reason:    "no access_token in auth file",
		}
	}

	// Note: full patrol candidates are enabled-only; cooldown recheck uses spending_only scope.
	live := g.storeGet(f.AuthIndex)

	baseURL := strings.TrimRight(strings.TrimSpace(af.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultProbeBaseURL
	}
	// Always inject Grok CLI identity; oauth files often omit headers and trigger HTTP 426.
	probeHeaders := mergeProbeHeaders(af.Headers)

	cfgProbe := g.Config()
	primary := strings.TrimSpace(cfgProbe.PatrolModel)
	if primary == "" {
		primary = DefaultPatrolModel
	}

	// Probe sequence: primary first; on 402 optionally try other models from this credential.
	tryModels := []string{primary}
	if cfgProbe.PatrolAutoModelSwitch {
		alts, _, _ := g.listModelsForToken(af.AccessToken, baseURL, probeHeaders, cfgProbe.PatrolProxyURL)
		for _, m := range alts {
			m = strings.TrimSpace(m)
			if m == "" || m == primary {
				continue
			}
			// Prefer free-ish ids first for recovery
			tryModels = append(tryModels, m)
		}
		// Prefer free-ish alternates after primary; cap primary+4
		free := []string{}
		other := []string{}
		for _, m := range tryModels[1:] {
			if strings.Contains(strings.ToLower(m), "free") {
				free = append(free, m)
			} else {
				other = append(other, m)
			}
		}
		tryModels = append([]string{primary}, append(free, other...)...)
		if len(tryModels) > 5 {
			tryModels = tryModels[:5]
		}
	}

	var (
		code     int
		bodyStr  string
		model    string
		lastErr  string
		tried    []string
	)
	for _, m := range tryModels {
		model = m
		tried = append(tried, m)
		var c int
		var b string
		var err error
		// Network / transient 5xx: retry same model with short backoff (do not burn alternate models).
		const maxNetAttempts = 3
		for attempt := 1; attempt <= maxNetAttempts; attempt++ {
			c, b, err = g.doChatProbe(client, baseURL, af.AccessToken, probeHeaders, m)
			if err != nil {
				lastErr = err.Error()
				netCode, netReason := classifyNetworkProbe(err)
				// canceled: no retry
				if netCode == -2 || attempt == maxNetAttempts {
					return probeResult{
						authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
						action: probeErrorKind(netCode, netReason),
						reason: fmt.Sprintf("probe network model=%s attempts=%d: %s", m, attempt, netReason),
						httpCode: netCode, modelUsed: m,
					}
				}
				time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
				continue
			}
			// transient upstream 5xx / 408 / 429-without-body handled later; soft-retry pure 5xx once more
			if (c == 408 || c == 500 || c == 502 || c == 503 || c == 504 || c == 520 || c == 522 || c == 524) && attempt < maxNetAttempts {
				lastErr = fmt.Sprintf("HTTP %d", c)
				time.Sleep(time.Duration(attempt) * 350 * time.Millisecond)
				continue
			}
			break
		}
		if err != nil {
			// defensive: loop should have returned
			netCode, netReason := classifyNetworkProbe(err)
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: probeErrorKind(netCode, netReason),
				reason: fmt.Sprintf("probe network model=%s: %s", m, netReason),
				httpCode: netCode, modelUsed: m,
			}
		}
		code, bodyStr = c, b
		// Success / free-window 429: stop model tries (classified below)
		if code == http.StatusOK || code == http.StatusTooManyRequests {
			break
		}
		if IsModelRegionUnavailable(code, bodyStr) {
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: probeErrorKind(code, "region"), reason: fmt.Sprintf("区域/模型不可用(不删) model=%s · %s", model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
			}
		}
		if IsPermissionDenied(code, bodyStr) || IsInvalidCredentials(code, bodyStr) {
			break
		}
		if IsSpendingLimitBlocked(code, bodyStr) {
			// try next model if auto-switch still has candidates
			if cfgProbe.PatrolAutoModelSwitch && m != tryModels[len(tryModels)-1] {
				g.logf("info", "patrol 402 on model=%s auth=%s, try next", m, f.AuthIndex)
				continue
			}
			break
		}
		// other codes: stop model tries; classified below
		break
	}
	_ = lastErr

	// Outcomes after model tries:
	// - 200: alive; re-enable if spending_limit cooldown
	// - 429 free-usage: cooldown (enabled) or re-enable spending_limit; never delete
	// - 402 spending: soft-disable (after auto-switch exhausted if enabled)
	// - 403/401 dead: delete
	// - region/404/5xx: error, no delete
	reenableIfSpending := func(reason string) probeResult {
		if live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Signal == "spending_limit" && live.Owner == Owner && !live.PreDisabled {
			if g.auth != nil {
				if _, err := g.auth.SetDisabled(f.AuthIndex, false); err != nil {
					return probeResult{
						authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
						action: "error", reason: fmt.Sprintf("re-enable failed: %v", err), httpCode: code, modelUsed: model,
					}
				}
			}
			_ = g.storeMarkActive(f.AuthIndex)
			g.logf("info", "patrol 探测恢复，已启用 spending_limit 账号 auth=%s reason=%s model=%s tried=%v", f.AuthIndex, reason, model, tried)
			g.NotifyWebhook("patrol_spending_recovered", map[string]any{
				"auth_index": f.AuthIndex, "file_name": f.Name, "account": f.Account,
				"http_code": code, "reason": reason, "model": model, "tried": tried,
			})
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "reenabled", reason: fmt.Sprintf("%s · model=%s tried=%v", reason, model, tried), httpCode: code, modelUsed: model,
			}
		}
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: "alive", reason: fmt.Sprintf("%s · model=%s tried=%v", reason, model, tried), httpCode: code, modelUsed: model,
		}
	}

	if code == http.StatusOK {
		return reenableIfSpending("200 OK")
	}
	if code == http.StatusTooManyRequests {
		// 429 never deletes.
		// spending_limit cooldown + 429 => credential still valid for free path => re-enable
		// free-usage short window on enabled account => plugin_auto cooldown (same as HandleUsage)
		if live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Signal == "spending_limit" && live.Owner == Owner && !live.PreDisabled {
			return reenableIfSpending("429 rate-limited (free quota window; not spending-limit)")
		}
		match429, ok429 := MatchShortWindowQuota(MatchInput{
			Provider: "xai", Failed: true, StatusCode: code, Body: bodyStr, Now: time.Now(),
			MaxResetSeconds: g.Config().MaxResetSeconds,
		})
		if !ok429 {
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "error", reason: fmt.Sprintf("429 未识别为短时额度信号 model=%s · %s", model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
			}
		}
		if actual, limit, pok := ParseFreeUsageTokens(bodyStr); pok {
			_ = g.storeObserveQuota(f.AuthIndex, actual, limit)
		}
		// Already our free-usage / short-window cooldown → extend (not spending_limit).
		if live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Owner == Owner && !live.PreDisabled && live.Signal != "spending_limit" {
			rec := *live
			rec.RecoverAtMS = match429.RecoverAt.UnixMilli()
			rec.LastProbeModel = model
			rec.Reason = match429.Reason
			rec.Signal = match429.Signal
			_ = g.storeUpsert(rec)
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "cooldown", reason: fmt.Sprintf("429 free-usage 延长冷却 model=%s · %s", model, match429.Reason), httpCode: code, modelUsed: model,
			}
		}
		if g.auth != nil && !f.Disabled {
			prev, err := g.auth.SetDisabled(f.AuthIndex, true)
			if err != nil {
				return probeResult{
					authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
					action: "error", reason: fmt.Sprintf("429 cooldown disable failed: %v", err), httpCode: code, modelUsed: model,
				}
			}
			if prev {
				return probeResult{
					authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
					action: "cooldown_skip", reason: "already disabled externally (429)", httpCode: code, modelUsed: model,
				}
			}
		}
		nowMS := time.Now().UnixMilli()
		_ = g.storeUpsert(AccountRecord{
			AuthIndex: f.AuthIndex, FileName: f.Name, Provider: "xai", Account: f.Account,
			DisableSource: SourcePluginAuto, State: StateAutoDisabled,
			RecoverAtMS: match429.RecoverAt.UnixMilli(), DisabledAtMS: nowMS,
			LastProbeModel: model,
			PreDisabled: false, Owner: Owner, Reason: match429.Reason, Signal: match429.Signal,
		})
		g.logf("warn", "patrol 429 free-usage 已冷却 auth=%s recover_at=%s model=%s", f.AuthIndex, match429.RecoverAt.Format(time.RFC3339), model)
		g.NotifyWebhook("patrol_free_usage_cooldown", map[string]any{
			"auth_index": f.AuthIndex, "file_name": f.Name, "account": f.Account,
			"http_code": code, "recover_at": match429.RecoverAt.Format(time.RFC3339), "model": model,
		})
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: "cooldown", reason: fmt.Sprintf("429 free-usage model=%s · %s", model, match429.Reason), httpCode: code, modelUsed: model,
		}
	}


	if IsSpendingLimitBlocked(code, bodyStr) {
		// Soft-disable only (distinct signal from 429 free-usage).
		match, ok := MatchSpendingLimitQuota(MatchInput{
			Provider: "xai", Failed: true, StatusCode: code, Body: bodyStr, Now: time.Now(),
			MaxResetSeconds: g.Config().MaxResetSeconds,
		})
		if !ok {
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "error", reason: "spending-limit body unmatched", httpCode: code,
			}
		}
		// Already under our spending cooldown → extend recover, keep disabled.
		if live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto &&
			live.Signal == "spending_limit" && live.Owner == Owner && !live.PreDisabled {
			rec := *live
			rec.RecoverAtMS = match.RecoverAt.UnixMilli()
			rec.LastProbeModel = model
			rec.Reason = match.Reason
			rec.Signal = match.Signal
			_ = g.storeUpsert(rec)
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "cooldown", reason: fmt.Sprintf("spending-limit still active model=%s tried=%v", model, tried), httpCode: code, modelUsed: model,
			}
		}
		// Disable if currently enabled.
		if g.auth != nil && !f.Disabled {
			prev, err := g.auth.SetDisabled(f.AuthIndex, true)
			if err != nil {
				return probeResult{
					authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
					action: "error", reason: fmt.Sprintf("disable failed: %v", err), httpCode: code,
				}
			}
			if prev {
				// External disable → do not own.
				return probeResult{
					authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
					action: "cooldown_skip", reason: "already disabled externally", httpCode: code,
				}
			}
		}
		nowMS := time.Now().UnixMilli()
		_ = g.storeUpsert(AccountRecord{
			AuthIndex: f.AuthIndex, FileName: f.Name, Provider: "xai", Account: f.Account,
			DisableSource: SourcePluginAuto, State: StateAutoDisabled,
			RecoverAtMS: match.RecoverAt.UnixMilli(), DisabledAtMS: nowMS,
			LastProbeModel: model,
			PreDisabled: false, Owner: Owner, Reason: match.Reason, Signal: match.Signal,
		})
		g.logf("warn", "patrol spending-limit 已禁用 auth=%s recover_at=%s", f.AuthIndex, match.RecoverAt.Format(time.RFC3339))
		g.NotifyWebhook("patrol_spending_disable", map[string]any{
			"auth_index": f.AuthIndex, "file_name": f.Name, "account": f.Account,
			"http_code": code, "recover_at": match.RecoverAt.Format(time.RFC3339),
		})
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: "cooldown", reason: fmt.Sprintf("402 spending-limit model=%s tried=%v · %s", model, tried, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
		}
	}
	if IsModelRegionUnavailable(code, bodyStr) {
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: probeErrorKind(code, "region"), reason: fmt.Sprintf("区域/模型不可用(不删) model=%s · %s", model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
		}
	}
	if IsPermissionDenied(code, bodyStr) || IsInvalidCredentials(code, bodyStr) {
		if err := g.auth.Delete(f.AuthIndex); err != nil {
			return probeResult{
				authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
				action: "error", reason: fmt.Sprintf("delete failed: %v", err), httpCode: code,
			}
		}
		_ = g.storeRemove(f.AuthIndex)
		if g.store != nil {
			_ = g.store.AppendDelete(DeleteEvent{
				AuthIndex: f.AuthIndex, FileName: f.Name, Account: f.Account, Provider: "xai",
				Reason: fmt.Sprintf("patrol: %s", truncate(bodyStr, 240)), DeletedAtMS: time.Now().UnixMilli(),
			})
		}
		g.logf("warn", "patrol 删除死号 auth=%s file=%s code=%d reason=%s", f.AuthIndex, f.Name, code, truncate(bodyStr, 120))
		g.NotifyWebhook("patrol_dead_credential_delete", map[string]any{
			"auth_index": f.AuthIndex, "file_name": f.Name, "account": f.Account,
			"http_code": code, "reason": truncate(bodyStr, 160),
		})
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: "deleted", reason: fmt.Sprintf("model=%s · %s", model, truncate(bodyStr, 180)), httpCode: code, modelUsed: model,
		}
	}

	// Other codes: probe failure / ambiguous — do NOT mark alive.
	// 404/405/422/5xx usually mean endpoint/model/proxy path wrong, not healthy credential.
	// 426 = Grok CLI client version identity rejected — probe-client issue, never delete.
	if isCLIVersionRejected(code, bodyStr) {
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: probeErrorKind(code, "cli version"), reason: fmt.Sprintf("CLI版本被拒 HTTP %d（非死号·请检查探测请求头） model=%s · %s", code, model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
		}
	}
	if live != nil && live.State == StateAutoDisabled && live.Signal == "spending_limit" {
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: probeErrorKind(code, bodyStr), reason: fmt.Sprintf("探测失败 HTTP %d（冷却号未恢复） model=%s · %s", code, model, truncate(bodyStr, 100)), httpCode: code, modelUsed: model,
		}
	}
	if code == http.StatusNotFound || code == http.StatusMethodNotAllowed || code == http.StatusUnprocessableEntity || code >= 500 {
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: probeErrorKind(code, bodyStr), reason: fmt.Sprintf("探测失败 HTTP %d model=%s · %s", code, model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
		}
	}
	if code >= 400 {
		return probeResult{
			authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
			action: probeErrorKind(code, bodyStr), reason: fmt.Sprintf("探测异常 HTTP %d model=%s · %s", code, model, truncate(bodyStr, 120)), httpCode: code, modelUsed: model,
		}
	}
	// 2xx/3xx non-200 already handled; treat remaining as soft-alive
	return probeResult{
		authIndex: f.AuthIndex, fileName: f.Name, account: f.Account,
		action: "alive", reason: fmt.Sprintf("HTTP %d model=%s", code, model), httpCode: code, modelUsed: model,
	}
}


// persistPatrolSnapshot writes current in-memory patrol counters/log to durable state.
// final=true forces write even if recently checkpointed.
func (g *Guard) persistPatrolSnapshot(final bool) {
	if g == nil || g.store == nil {
		return
	}
	g.patrol.mu.Lock()
	now := time.Now().UnixMilli()
	if !final && g.patrol.lastPersistMS > 0 && now-g.patrol.lastPersistMS < 2000 {
		g.patrol.mu.Unlock()
		return
	}
	byHTTP := map[string]int{}
	for code, n := range g.patrol.byHTTP {
		byHTTP[fmt.Sprintf("%d", code)] = n
	}
	byAction := map[string]int{}
	for k, n := range g.patrol.byAction {
		byAction[k] = n
	}
	log := make([]PatrolLogEntry, len(g.patrol.lastSweepLog))
	copy(log, g.patrol.lastSweepLog)
	snap := PatrolSnapshot{
		Running: g.patrol.running, StartedAtMS: g.patrol.startedAtMS, CompletedAtMS: g.patrol.completedAtMS,
		TotalCandidates: g.patrol.totalCandidates, TotalProbed: g.patrol.totalProbed,
		TotalDeleted: g.patrol.totalDeleted, TotalErrors: g.patrol.totalErrors,
		TotalAlive: g.patrol.totalAlive, TotalSkipped: g.patrol.totalSkipped,
		TotalCooldown: g.patrol.totalCooldown, Total429CD: g.patrol.total429CD, TotalSpendCD: g.patrol.totalSpendCD,
		TotalReenabled: g.patrol.totalReenabled, ByHTTP: byHTTP, ByAction: byAction,
		Workers: g.patrol.workers, LastError: g.patrol.lastError, Scope: g.patrol.scope,
		RecentLog: log, SavedAtMS: now,
	}
	if final {
		snap.Running = false
	}
	g.patrol.lastPersistMS = now
	g.patrol.mu.Unlock()
	if err := g.store.SaveLastPatrol(snap); err != nil {
		g.logf("warn", "persist patrol snapshot failed: %v", err)
	}
}

// hydratePatrolFromStore loads last durable patrol into memory if empty (plugin restart).
func (g *Guard) hydratePatrolFromStore() {
	if g == nil || g.store == nil {
		return
	}
	g.patrol.mu.Lock()
	need := len(g.patrol.lastSweepLog) == 0 && g.patrol.totalProbed == 0 && !g.patrol.running
	g.patrol.mu.Unlock()
	if !need {
		return
	}
	snap := g.store.GetLastPatrol()
	if snap == nil {
		return
	}
	g.patrol.mu.Lock()
	// re-check after lock
	if len(g.patrol.lastSweepLog) > 0 || g.patrol.totalProbed > 0 || g.patrol.running {
		g.patrol.mu.Unlock()
		return
	}
	g.patrol.running = false // never resume mid-sweep after restart
	g.patrol.startedAtMS = snap.StartedAtMS
	g.patrol.completedAtMS = snap.CompletedAtMS
	g.patrol.totalCandidates = snap.TotalCandidates
	g.patrol.totalProbed = snap.TotalProbed
	g.patrol.totalDeleted = snap.TotalDeleted
	g.patrol.totalErrors = snap.TotalErrors
	g.patrol.totalAlive = snap.TotalAlive
	g.patrol.totalSkipped = snap.TotalSkipped
	g.patrol.totalCooldown = snap.TotalCooldown
	g.patrol.total429CD = snap.Total429CD
	g.patrol.totalSpendCD = snap.TotalSpendCD
	g.patrol.totalReenabled = snap.TotalReenabled
	g.patrol.workers = snap.Workers
	g.patrol.lastError = snap.LastError
	g.patrol.scope = snap.Scope
	g.patrol.byHTTP = map[int]int{}
	for k, n := range snap.ByHTTP {
		var code int
		fmt.Sscanf(k, "%d", &code)
		g.patrol.byHTTP[code] = n
	}
	g.patrol.byAction = map[string]int{}
	for k, n := range snap.ByAction {
		g.patrol.byAction[k] = n
	}
	g.patrol.lastSweepLog = make([]patrolLogEntry, len(snap.RecentLog))
	copy(g.patrol.lastSweepLog, snap.RecentLog)
	g.patrol.mu.Unlock()
}

// PatrolStatus returns the current patrol state for the UI.
func (g *Guard) PatrolStatus() PatrolStatus {
	g.hydratePatrolFromStore()
	g.patrol.mu.Lock()
	defer g.patrol.mu.Unlock()

	log := make([]patrolLogEntry, len(g.patrol.lastSweepLog))
	copy(log, g.patrol.lastSweepLog)
	// newest first for UI
	for i, j := 0, len(log)-1; i < j; i, j = i+1, j-1 {
		log[i], log[j] = log[j], log[i]
	}

	byHTTP := map[string]int{}
	for code, n := range g.patrol.byHTTP {
		byHTTP[fmt.Sprintf("%d", code)] = n
	}
	byAction := map[string]int{}
	for k, n := range g.patrol.byAction {
		byAction[k] = n
	}
	return PatrolStatus{
		Running:         g.patrol.running,
		StartedAtMS:     g.patrol.startedAtMS,
		CompletedAtMS:   g.patrol.completedAtMS,
		TotalCandidates: g.patrol.totalCandidates,
		TotalProbed:     g.patrol.totalProbed,
		TotalDeleted:    g.patrol.totalDeleted,
		TotalErrors:     g.patrol.totalErrors,
		TotalAlive:      g.patrol.totalAlive,
		TotalSkipped:    g.patrol.totalSkipped,
		TotalCooldown:         g.patrol.totalCooldown,
		Total429CD:      g.patrol.total429CD,
		TotalSpendCD:    g.patrol.totalSpendCD,
		TotalReenabled:  g.patrol.totalReenabled,
		ByHTTP:          byHTTP,
		ByAction:        byAction,
		Workers:         g.patrol.workers,
		WorkersMax:      g.patrol.workersMax,
		WorkersMin:      g.patrol.workersMin,
		Load1:           g.patrol.load1,
		ScaleReason:     g.patrol.scaleReason,
		LastError:       g.patrol.lastError,
		Scope:           g.patrol.scope,
		RecentLog:       log,
	}
}

// PatrolStop signals an in-progress sweep to stop after current in-flight probes.
func (g *Guard) PatrolStop() {
	g.patrol.mu.Lock()
	g.patrol.stopRequested = true
	g.patrol.mu.Unlock()
}

// PatrolRunOnce triggers an async manual sweep if not already running.
func (g *Guard) PatrolRunSpendingOnly() PatrolStatus {
	g.patrol.mu.Lock()
	if g.patrol.running {
		g.patrol.mu.Unlock()
		return g.PatrolStatus()
	}
	g.patrol.mu.Unlock()
	go g.PatrolSweep(PatrolOptions{Scope: "spending_only"})
	for i := 0; i < 20; i++ {
		time.Sleep(25 * time.Millisecond)
		st := g.PatrolStatus()
		if st.Running || st.CompletedAtMS > 0 {
			return st
		}
	}
	return g.PatrolStatus()
}

func (g *Guard) PatrolRunOnce() PatrolStatus {
	g.patrol.mu.Lock()
	if g.patrol.running {
		g.patrol.mu.Unlock()
		return g.PatrolStatus()
	}
	g.patrol.mu.Unlock()
	// Mark running ASAP so UI sees activity before goroutine starts.
	// PatrolSweep re-checks and sets counters.
	go g.PatrolSweep(PatrolOptions{Scope: "all"})
	// Small spin so first status after POST often shows running=true.
	for i := 0; i < 20; i++ {
		st := g.PatrolStatus()
		if st.Running || st.LastError != "" {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	return g.PatrolStatus()
}



// doChatProbe sends a minimal upstream probe for the given model.
// Prefer CPA/xAI executor path POST /responses (Grok CLI chat-proxy + api.x.ai);
// fall back to /chat/completions only when /responses is missing (404/405).
func (g *Guard) doChatProbe(client *http.Client, baseURL, token string, headers map[string]string, model string) (int, string, error) {
	base := strings.TrimRight(baseURL, "/")
	// Responses API shape used by CLIProxyAPI xai_executor (input can be string).
	respPayload, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "ping",
		"max_output_tokens": 1,
		"stream":            false,
	})
	code, body, err := g.doProbePOST(client, base+"/responses", token, headers, respPayload)
	if err != nil {
		return 0, "", err
	}
	// Endpoint missing / wrong method → try OpenAI chat completions.
	if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		chatPayload, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		c2, b2, err2 := g.doProbePOST(client, base+"/chat/completions", token, headers, chatPayload)
		if err2 != nil {
			return code, body, nil // keep responses result if fallback network fails
		}
		if c2 != http.StatusNotFound && c2 != http.StatusMethodNotAllowed {
			return c2, b2, nil
		}
		return code, body, nil
	}
	return code, body, nil
}

func (g *Guard) doProbePOST(client *http.Client, url, token string, headers map[string]string, payload []byte) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// listModelsForToken GETs /models for one token (no auth-file scan).
func (g *Guard) listModelsForToken(token, baseURL string, headers map[string]string, proxyURL string) ([]string, string, string) {
	client := g.newPatrolHTTPClient(15*time.Second, proxyURL)
	return g.listModelsForTokenWithClient(client, token, baseURL, headers)
}

// ListPatrolModels uses one enabled xAI credential to GET /models from upstream.
// Falls back to SuggestedPatrolModels when no credential or request fails.
func (g *Guard) ListPatrolModels() (models []string, source string, errMsg string) {
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, id)
	}
	for _, s := range SuggestedPatrolModels {
		add(s)
	}
	source = "suggested"
	cfg := g.Config()
	add(cfg.PatrolModel)
	add(DefaultPatrolModel)
	if g.auth == nil {
		return models, source, "no auth lookup"
	}
	files, err := g.auth.List()
	if err != nil {
		return models, source, err.Error()
	}
	authDir := strings.TrimSpace(cfg.PatrolAuthDir)
	if authDir == "" {
		return models, source, "patrol_auth_dir empty"
	}
	// Try up to 3 enabled xAI credentials (first may be bad token).
	tried := 0
	var lastErr string
	client := g.newPatrolHTTPClient(15*time.Second, cfg.PatrolProxyURL)
	for i := range files {
		f := &files[i]
		if !IsXAIProvider(f.Provider, "") || f.Disabled {
			continue
		}
		tried++
		if tried > 3 {
			break
		}
		filePath := filepath.Join(authDir, f.Name)
		raw, err := os.ReadFile(filePath)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		var af authFileJSON
		if err := json.Unmarshal(raw, &af); err != nil {
			lastErr = err.Error()
			continue
		}
		if af.AccessToken == "" {
			lastErr = "no access_token"
			continue
		}
		baseURL := strings.TrimRight(strings.TrimSpace(af.BaseURL), "/")
		if baseURL == "" {
			baseURL = DefaultProbeBaseURL
		}
		ids, src, e := g.listModelsForTokenWithClient(client, af.AccessToken, baseURL, mergeProbeHeaders(af.Headers))
		if e != "" {
			lastErr = e
			continue
		}
		for _, id := range ids {
			add(id)
		}
		source = src + ":" + f.Name
		return models, source, ""
	}
	if lastErr != "" {
		return models, source, lastErr
	}
	if tried == 0 {
		return models, source, "no enabled xAI credential"
	}
	return models, source, lastErr
}

func (g *Guard) listModelsForTokenWithClient(client *http.Client, token, baseURL string, headers map[string]string) ([]string, string, string) {
	var models []string
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, id)
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultProbeBaseURL
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, "error", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "error", err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "error", fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	// OpenAI-style {data:[{id:...}]}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		for _, m := range parsed.Data {
			add(m.ID)
		}
		for _, m := range parsed.Models {
			add(m.ID)
		}
	}
	if len(models) == 0 {
		var arr []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &arr); err == nil {
			for _, m := range arr {
				add(m.ID)
			}
		}
	}
	if len(models) == 0 {
		// xAI sometimes nests differently — scan string ids loosely
		var rawObj map[string]any
		if err := json.Unmarshal(body, &rawObj); err == nil {
			if data, ok := rawObj["data"].([]any); ok {
				for _, it := range data {
					if m, ok := it.(map[string]any); ok {
						if id, ok := m["id"].(string); ok {
							add(id)
						}
					}
				}
			}
		}
	}
	if len(models) == 0 {
		return nil, "error", "empty model list"
	}
	return models, "credential", ""
}

