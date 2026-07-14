package xaiquota

import (
	"testing"
	"time"
)

func TestParseFreeUsageTokens(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1091108/1000000."}`
	a, l, ok := ParseFreeUsageTokens(body)
	if !ok || a != 1091108 || l != 1000000 {
		t.Fatalf("got ok=%v actual=%d limit=%d", ok, a, l)
	}
}

func TestBuildMetricsViewKnownOnly(t *testing.T) {
	st := UsageStats{
		DayKey:    "2026-07-11",
		UsedToday: 100,
		UsedTotal: 200,
		QuotaByAuth: map[string]*AccountQuotaSnapshot{
			"a1":   {AuthIndex: "a1", Actual: 900000, Limit: 1000000},
			"gone": {AuthIndex: "gone", Actual: 500000, Limit: 1000000},
		},
		DefaultLimitPerAcct: DefaultFreeLimit,
	}
	v := BuildMetricsView(3, 2, 1, st)
	if v.XAITotal != 3 || v.XAIEnabled != 2 || v.XAIDisabled != 1 {
		t.Fatalf("inventory bad: %+v", v)
	}
	if v.QuotaKnownAccounts != 2 {
		t.Fatalf("known without live filter=%d want 2", v.QuotaKnownAccounts)
	}
	if v.QuotaTotalEst != 2000000 {
		t.Fatalf("known-only est=%d want 2000000", v.QuotaTotalEst)
	}
	if v.UnobservedAccounts != 0 {
		t.Fatalf("unobserved=%d want 0 (known 2 >= enabled 2)", v.UnobservedAccounts)
	}
	if v.UsedTodayDisplay != 100 || v.UsedTotalDisplay != 200 {
		t.Fatalf("display today=%d total=%d want 100/200", v.UsedTodayDisplay, v.UsedTotalDisplay)
	}
	if v.RollingUsedKnown != 1400000 || v.RollingLimitKnown != 2000000 {
		t.Fatalf("rolling bad: used=%d limit=%d", v.RollingUsedKnown, v.RollingLimitKnown)
	}
}

func TestBuildMetricsViewLiveFilterAndDailyPool(t *testing.T) {
	st := UsageStats{
		UsedToday: 50,
		UsedTotal: 80,
		QuotaByAuth: map[string]*AccountQuotaSnapshot{
			"a1":   {AuthIndex: "a1", Actual: 100000, Limit: 1000000},
			"gone": {AuthIndex: "gone", Actual: 999999, Limit: 1000000},
		},
		DefaultLimitPerAcct: DefaultFreeLimit,
	}
	live := map[string]bool{"a1": true}
	v := BuildMetricsViewOpts(10, 5, 5, st, true, live)
	if v.QuotaTotalEst != 5*DefaultFreeLimit {
		t.Fatalf("daily pool est=%d want %d (enabled*2M)", v.QuotaTotalEst, 5*DefaultFreeLimit)
	}
	if v.QuotaKnownAccounts != 1 || v.RollingAccounts != 1 {
		t.Fatalf("live filter known=%d rolling_acc=%d", v.QuotaKnownAccounts, v.RollingAccounts)
	}
	if v.RollingUsedKnown != 100000 || v.RollingLimitKnown != 1000000 {
		t.Fatalf("rolling should exclude gone: used=%d limit=%d", v.RollingUsedKnown, v.RollingLimitKnown)
	}
	if v.UnobservedAccounts != 4 {
		t.Fatalf("unobserved enabled=%d want 4", v.UnobservedAccounts)
	}
	if v.UsedTodayDisplay != 50 || v.UsedTotalDisplay != 80 {
		t.Fatalf("real display today=%d total=%d", v.UsedTodayDisplay, v.UsedTotalDisplay)
	}
	v0 := BuildMetricsViewOpts(10, 0, 10, st, true, live)
	if v0.QuotaTotalEst != 0 {
		t.Fatalf("enabled=0 pool=%d want 0", v0.QuotaTotalEst)
	}
}

func TestObserveFreeQuotaSnapshotOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir + "/st.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.ObserveFreeQuota("a1", 1000, 1000000, now); err != nil {
		t.Fatal(err)
	}
	st := s.GetUsageStats()
	if st.UsedToday != 0 || st.UsedTotal != 0 {
		t.Fatalf("observe must not touch calendar used: today=%d total=%d", st.UsedToday, st.UsedTotal)
	}
	if st.QuotaByAuth["a1"] == nil || st.QuotaByAuth["a1"].Actual != 1000 {
		t.Fatalf("snapshot missing: %+v", st.QuotaByAuth["a1"])
	}
	if err := s.ObserveFreeQuota("a1", 1500, 1000000, now); err != nil {
		t.Fatal(err)
	}
	st = s.GetUsageStats()
	if st.UsedToday != 0 || st.UsedTotal != 0 {
		t.Fatalf("second observe still no delta into used: today=%d total=%d", st.UsedToday, st.UsedTotal)
	}
	if st.QuotaByAuth["a1"].Actual != 1500 {
		t.Fatalf("snapshot actual=%d want 1500", st.QuotaByAuth["a1"].Actual)
	}
}

func TestSyncAuthCounters(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir + "/st.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.SyncAuthCounters(10, 1, 8000, now); err != nil {
		t.Fatal(err)
	}
	st := s.GetUsageStats()
	if st.EstimatedToday != 0 || st.RequestsToday != 0 {
		t.Fatalf("baseline should not count history: %+v", st)
	}
	if err := s.SyncAuthCounters(12, 1, 8000, now); err != nil {
		t.Fatal(err)
	}
	st = s.GetUsageStats()
	if st.RequestsToday != 2 || st.EstimatedToday != 16000 {
		t.Fatalf("delta bad today req=%d est=%d", st.RequestsToday, st.EstimatedToday)
	}
}

func TestAddUsageEventPerAuthAndZeroStreak(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir + "/st.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = s.AddUsageEvent("a1", 50000, false, now)
	_ = s.AddUsageEvent("a1", 0, false, now)
	_ = s.AddUsageEvent("a1", 0, false, now)
	_ = s.AddUsageEvent("a1", 0, false, now)
	_ = s.AddUsageEvent("a1", 0, false, now)
	_ = s.AddUsageEvent("a1", 0, false, now)
	st := s.GetUsageStats()
	if st.UsedToday != 50000 {
		t.Fatalf("used=%d", st.UsedToday)
	}
	u := st.UsageByAuth["a1"]
	if u == nil || u.RequestsToday != 6 || u.ZeroTokenOK != 5 {
		t.Fatalf("per-auth bad: %+v", u)
	}
	v := BuildMetricsView(1, 1, 0, st)
	if !v.DetailMissingAlert || v.ZeroTokenStreak < 5 {
		t.Fatalf("alert expected: %+v", v)
	}
	_ = s.AddUsageEvent("a1", 100, false, now)
	st = s.GetUsageStats()
	v = BuildMetricsView(1, 1, 0, st)
	if v.DetailMissingAlert || v.ZeroTokenStreak != 0 {
		t.Fatalf("streak should clear: %+v", v)
	}
	if v.UsedTodayDisplay != 50100 {
		t.Fatalf("display=%d want 50100", v.UsedTodayDisplay)
	}
}

func TestBuildMetricsViewDailyPoolEnabledOnly(t *testing.T) {
	st := UsageStats{DefaultLimitPerAcct: DefaultFreeLimit}
	v := BuildMetricsViewOpts(522, 500, 22, st, true, nil)
	if v.QuotaTotalEst != 500*DefaultFreeLimit {
		t.Fatalf("daily pool est=%d want %d", v.QuotaTotalEst, 500*DefaultFreeLimit)
	}
	if v.UnobservedAccounts != 500 {
		t.Fatalf("unobs=%d want 500 (all enabled unobserved)", v.UnobservedAccounts)
	}
	v2 := BuildMetricsViewOpts(22, 0, 22, st, true, nil)
	if v2.QuotaTotalEst != 0 {
		t.Fatalf("disabled-only pool=%d", v2.QuotaTotalEst)
	}
}


func TestResetCalendarToday(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir + "/st.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = s.AddUsageEvent("a1", 1000, false, now)
	_ = s.ObserveFreeQuota("a1", 900000, 1000000, now)
	st := s.GetUsageStats()
	if st.UsedToday != 1000 {
		t.Fatalf("used=%d", st.UsedToday)
	}
	if err := s.ResetCalendarToday(now, "test"); err != nil {
		t.Fatal(err)
	}
	st = s.GetUsageStats()
	if st.UsedToday != 0 || st.RequestsToday != 0 {
		t.Fatalf("today not cleared: %+v", st)
	}
	if st.UsedTotal != 1000 {
		t.Fatalf("total should keep 1000 got %d", st.UsedTotal)
	}
	if st.QuotaByAuth["a1"] == nil || st.QuotaByAuth["a1"].Actual != 900000 {
		t.Fatalf("snapshot should remain: %+v", st.QuotaByAuth["a1"])
	}
}
