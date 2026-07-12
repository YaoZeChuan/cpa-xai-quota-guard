package xaiquota

import (
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
	workers       int
	lastError     string
	lastSweepLog  []patrolLogEntry
	stopRequested bool
}

type patrolLogEntry struct {
	TimeMS    int64  `json:"time_ms"`
	AuthIndex string `json:"auth_index"`
	FileName  string `json:"file_name"`
	Account   string `json:"account"`
	Action    string `json:"action"` // "alive", "deleted", "error", "cooldown_skip"
	Reason    string `json:"reason"`
	HTTPCode  int    `json:"http_code,omitempty"`
}

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
	Workers         int              `json:"workers"`
	LastError       string           `json:"last_error,omitempty"`
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

// probeResult holds the outcome of probing one credential.
type probeResult struct {
	authIndex string
	fileName  string
	account   string
	action    string // "alive", "deleted", "error", "cooldown_skip"
	reason    string
	httpCode  int
}

// PatrolSweep iterates all enabled xAI auth files with a worker pool,
// probes the upstream directly, and deletes dead credentials.
func (g *Guard) PatrolSweep() PatrolStatus {
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
	g.patrol.workers = 0
	g.patrol.lastError = ""
	g.patrol.lastSweepLog = nil
	g.patrol.stopRequested = false
	g.patrol.mu.Unlock()

	defer func() {
		g.patrol.mu.Lock()
		g.patrol.running = false
		g.patrol.completedAtMS = time.Now().UnixMilli()
		g.patrol.mu.Unlock()
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

	// Full sweep of enabled xAI only; no failed/success filter.
	candidates := make([]AuthFile, 0, len(files))
	for _, f := range files {
		if !IsXAIProvider(f.Provider, "") {
			continue
		}
		if f.Disabled {
			continue
		}
		candidates = append(candidates, f)
	}

	batchLimit := cfg.PatrolBatchSize
	if batchLimit > 0 && batchLimit < len(candidates) {
		candidates = candidates[:batchLimit]
	}

	workers := cfg.PatrolConcurrency
	if workers <= 0 {
		workers = 8
	}
	if workers > len(candidates) && len(candidates) > 0 {
		workers = len(candidates)
	}

	g.patrol.mu.Lock()
	g.patrol.totalCandidates = len(candidates)
	g.patrol.workers = workers
	g.patrol.mu.Unlock()

	if len(candidates) == 0 {
		return g.PatrolStatus()
	}

	probeTimeout := time.Duration(cfg.PatrolTimeout) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}

	client := g.newPatrolHTTPClient(probeTimeout, cfg.PatrolProxyURL)

	jobs := make(chan AuthFile, workers*2)
	var wg sync.WaitGroup
	var stopFlag int32

	// Watch stop request in a light loop via atomic.
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

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if atomic.LoadInt32(&stopFlag) == 1 {
					return
				}
				result := g.probeOneCredential(f, authDir, client)
				g.recordProbeResult(result)
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

	return g.PatrolStatus()
}

func (g *Guard) setPatrolError(msg string) {
	g.patrol.mu.Lock()
	g.patrol.lastError = msg
	g.patrol.mu.Unlock()
}

func (g *Guard) newPatrolHTTPClient(timeout time.Duration, proxyURL string) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
	}
	if strings.TrimSpace(proxyURL) != "" {
		if u, err := url.Parse(strings.TrimSpace(proxyURL)); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func (g *Guard) recordProbeResult(r probeResult) {
	g.patrol.mu.Lock()
	g.patrol.totalProbed++
	switch r.action {
	case "deleted":
		g.patrol.totalDeleted++
	case "error":
		g.patrol.totalErrors++
	case "cooldown_skip":
		g.patrol.totalSkipped++
	case "alive":
		g.patrol.totalAlive++
	default:
		g.patrol.totalAlive++
	}
	entry := patrolLogEntry{
		TimeMS:    time.Now().UnixMilli(),
		AuthIndex: r.authIndex,
		FileName:  r.fileName,
		Account:   r.account,
		Action:    r.action,
		Reason:    r.reason,
		HTTPCode:  r.httpCode,
	}
	if len(g.patrol.lastSweepLog) >= 500 {
		g.patrol.lastSweepLog = g.patrol.lastSweepLog[len(g.patrol.lastSweepLog)-499:]
	}
	g.patrol.lastSweepLog = append(g.patrol.lastSweepLog, entry)
	g.patrol.mu.Unlock()
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

	// Skip if currently in plugin_auto cooldown.
	live := g.storeGet(f.AuthIndex)
	if live != nil && live.State == StateAutoDisabled && live.DisableSource == SourcePluginAuto {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "cooldown_skip",
			reason:    "currently in plugin_auto cooldown",
		}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(af.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.x.ai/v1"
	}

	probeBody := `{"model":"grok-3","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", strings.NewReader(probeBody))
	if err != nil {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "error",
			reason:    fmt.Sprintf("build request: %v", err),
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+af.AccessToken)
	for k, v := range af.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "error",
			reason:    fmt.Sprintf("probe request failed (network): %v", err),
		}
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	code := resp.StatusCode

	// 200/429/5xx = alive; 403/401/402 dead-signal = delete.
	if code == http.StatusOK {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "alive",
			reason:    "200 OK",
			httpCode:  code,
		}
	}
	if code == http.StatusTooManyRequests {
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "alive",
			reason:    "429 rate-limited (quota exhausted, not dead)",
			httpCode:  code,
		}
	}
	if IsPermissionDenied(code, bodyStr) || IsInvalidCredentials(code, bodyStr) || IsSpendingLimitBlocked(code, bodyStr) {
		if err := g.auth.Delete(f.AuthIndex); err != nil {
			return probeResult{
				authIndex: f.AuthIndex,
				fileName:  f.Name,
				account:   f.Account,
				action:    "error",
				reason:    fmt.Sprintf("delete failed: %v", err),
				httpCode:  code,
			}
		}
		_ = g.storeRemove(f.AuthIndex)
		if g.store != nil {
			_ = g.store.AppendDelete(DeleteEvent{
				AuthIndex:   f.AuthIndex,
				FileName:    f.Name,
				Account:     f.Account,
				Provider:    "xai",
				Reason:      fmt.Sprintf("patrol: %s", truncate(bodyStr, 240)),
				DeletedAtMS: time.Now().UnixMilli(),
			})
		}
		g.logf("warn", "patrol 删除死号 auth=%s file=%s code=%d reason=%s", f.AuthIndex, f.Name, code, truncate(bodyStr, 120))
		g.NotifyWebhook("patrol_dead_credential_delete", map[string]any{
			"auth_index": f.AuthIndex,
			"file_name":  f.Name,
			"account":    f.Account,
			"http_code":  code,
			"reason":     truncate(bodyStr, 160),
		})
		return probeResult{
			authIndex: f.AuthIndex,
			fileName:  f.Name,
			account:   f.Account,
			action:    "deleted",
			reason:    truncate(bodyStr, 200),
			httpCode:  code,
		}
	}

	return probeResult{
		authIndex: f.AuthIndex,
		fileName:  f.Name,
		account:   f.Account,
		action:    "alive",
		reason:    fmt.Sprintf("HTTP %d (not a dead-credential signal)", code),
		httpCode:  code,
	}
}

// PatrolStatus returns the current patrol state for the UI.
func (g *Guard) PatrolStatus() PatrolStatus {
	g.patrol.mu.Lock()
	defer g.patrol.mu.Unlock()

	log := make([]patrolLogEntry, len(g.patrol.lastSweepLog))
	copy(log, g.patrol.lastSweepLog)
	// newest first for UI
	for i, j := 0, len(log)-1; i < j; i, j = i+1, j-1 {
		log[i], log[j] = log[j], log[i]
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
		Workers:         g.patrol.workers,
		LastError:       g.patrol.lastError,
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
func (g *Guard) PatrolRunOnce() PatrolStatus {
	g.patrol.mu.Lock()
	if g.patrol.running {
		g.patrol.mu.Unlock()
		return g.PatrolStatus()
	}
	g.patrol.mu.Unlock()
	// Mark running ASAP so UI sees activity before goroutine starts.
	// PatrolSweep re-checks and sets counters.
	go g.PatrolSweep()
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