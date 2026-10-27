package auth

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var quotaTestNow = time.Unix(1793100000, 0)

type quotaTestClock struct{ now time.Time }

func (c *quotaTestClock) Now() time.Time { return c.now }

func newQuotaTestSelector(clock *quotaTestClock) *QuotaPacingSelector {
	return &QuotaPacingSelector{now: clock.Now}
}

func unixString(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// claudeQuotaAuth builds a Claude OAuth credential whose last response reported the given
// 5h and 7d utilization and time until each reset.
func claudeQuotaAuth(id string, observedAt time.Time, used5h float64, reset5hIn time.Duration, used7d float64, reset7dIn time.Duration) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: observedAt,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(used5h, 'f', 2, 64),
				"Anthropic-Ratelimit-Unified-5h-Reset":       unixString(observedAt.Add(reset5hIn)),
				"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(used7d, 'f', 2, 64),
				"Anthropic-Ratelimit-Unified-7d-Reset":       unixString(observedAt.Add(reset7dIn)),
				"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
			},
		},
	}
}

func sessionOpts(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: id}}
}

func mustPick(t *testing.T, selector Selector, opts cliproxyexecutor.Options, auths ...*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() returned nil auth")
	}
	return got.ID
}

func TestParseQuotaWindows_Claude(t *testing.T) {
	t.Parallel()
	auth := claudeQuotaAuth("a", quotaTestNow, 0.32, 2*time.Hour, 0.48, 21*time.Hour)
	windows := parseQuotaWindows("claude", auth.Quota.Signals, auth.Quota.ObservedAt)
	if windows == nil || windows.short == nil || windows.long == nil {
		t.Fatalf("parseQuotaWindows() = %+v, want short and long windows", windows)
	}
	if windows.short.utilization != 0.32 || windows.short.length != 5*time.Hour || !windows.short.resetAt.Equal(quotaTestNow.Add(2*time.Hour)) {
		t.Fatalf("short window = %+v", windows.short)
	}
	if windows.long.utilization != 0.48 || windows.long.length != 7*24*time.Hour {
		t.Fatalf("long window = %+v", windows.long)
	}
}

func TestParseQuotaWindows_ClaudeRejectedWithoutUtilization(t *testing.T) {
	t.Parallel()
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":  unixString(quotaTestNow.Add(time.Hour)),
	}
	windows := parseQuotaWindows("claude", signals, quotaTestNow)
	if windows == nil || windows.short == nil || !windows.short.rejected || windows.short.utilization != 1 {
		t.Fatalf("parseQuotaWindows() = %+v, want a rejected full short window", windows)
	}
}

func TestParseQuotaWindows_RejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, signals := range []map[string]string{
		{"Anthropic-Ratelimit-Unified-5h-Utilization": "NaN", "Anthropic-Ratelimit-Unified-5h-Reset": unixString(quotaTestNow)},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5", "Anthropic-Ratelimit-Unified-5h-Reset": "3600"},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": "-1", "Anthropic-Ratelimit-Unified-5h-Reset": unixString(quotaTestNow)},
		{"Retry-After": "30"},
	} {
		if windows := parseQuotaWindows("claude", signals, quotaTestNow); windows != nil {
			t.Fatalf("parseQuotaWindows(%v) = %+v, want nil", signals, windows)
		}
	}
}

func TestParseQuotaWindows_Codex(t *testing.T) {
	t.Parallel()
	signals := map[string]string{
		"X-Codex-Primary-Used-Percent":          "40",
		"X-Codex-Primary-Window-Minutes":        "300",
		"X-Codex-Primary-Reset-After-Seconds":   "3600",
		"X-Codex-Secondary-Used-Percent":        "8",
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-At":            strconv.FormatInt(quotaTestNow.Add(48*time.Hour).UnixMilli(), 10),
		"X-Codex-Secondary-Reset-After-Seconds": "1",
	}
	windows := parseQuotaWindows("codex", signals, quotaTestNow)
	if windows == nil || windows.short == nil || windows.long == nil {
		t.Fatalf("parseQuotaWindows() = %+v, want short and long", windows)
	}
	if windows.short.utilization != 0.4 || !windows.short.resetAt.Equal(quotaTestNow.Add(time.Hour)) {
		t.Fatalf("short = %+v", windows.short)
	}
	if windows.long.utilization != 0.08 || !windows.long.resetAt.Equal(quotaTestNow.Add(48*time.Hour)) {
		t.Fatalf("long = %+v (Reset-At in milliseconds must win over Reset-After-Seconds)", windows.long)
	}

	weeklyOnly := map[string]string{
		"X-Codex-Primary-Used-Percent":     "8",
		"X-Codex-Primary-Window-Minutes":   "10080",
		"X-Codex-Primary-Reset-At":         unixString(quotaTestNow.Add(100 * time.Hour)),
		"X-Codex-Secondary-Used-Percent":   "0",
		"X-Codex-Secondary-Window-Minutes": "0",
	}
	windows = parseQuotaWindows("codex", weeklyOnly, quotaTestNow)
	if windows == nil || windows.short != nil || windows.long == nil {
		t.Fatalf("weekly-only codex = %+v, want only a long window", windows)
	}
}

func TestQuotaPacing_PrefersWeeklyQuotaAtRisk(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	// A: half its week left, resets in 21h. B: 62% left, resets in 77h.
	a := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	for i := 0; i < 3; i++ {
		if got := mustPick(t, selector, cliproxyexecutor.Options{}, b, a); got != "a" {
			t.Fatalf("pick %d = %q, want a", i, got)
		}
	}
}

func TestQuotaPacing_AvoidsShortWindowOnCourseToRunOut(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	// A is the weekly priority, but used 70% of its 5h window in the 1.5h since it opened.
	a := claudeQuotaAuth("a", quotaTestNow, 0.70, 3*time.Hour+30*time.Minute, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "b" {
		t.Fatalf("pick = %q, want b", got)
	}
}

func TestQuotaPacing_ShortWindowSlopeFromObservations(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	reset := quotaTestNow.Add(3 * time.Hour)
	// A's average since its window opened looks calm (20% over 2h), so it is picked.
	a := claudeQuotaAuth("a", quotaTestNow, 0.20, reset.Sub(quotaTestNow), 0.50, 21*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	// Ten minutes later it has burned another 20%: at that rate it runs out long before reset.
	clock.now = quotaTestNow.Add(10 * time.Minute)
	a = claudeQuotaAuth("a", clock.now, 0.40, reset.Sub(clock.now), 0.50, 21*time.Hour-10*time.Minute)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "b" {
		t.Fatalf("second pick = %q, want b once A's observed burn rate is too high", got)
	}
}

func TestQuotaPacing_LoadAlreadyBoundCountsTowardWeeklyPace(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	b := claudeQuotaAuth("b", quotaTestNow, 0.05, 4*time.Hour, 0.38, 77*time.Hour)
	a := claudeQuotaAuth("a", quotaTestNow, 0.05, 4*time.Hour, 0.50, 21*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	// An hour later A's bound sessions burn 4% of its week per hour: enough to finish its
	// remaining 46% before the reset, so new work should help spend B instead.
	clock.now = quotaTestNow.Add(time.Hour)
	a = claudeQuotaAuth("a", clock.now, 0.30, 3*time.Hour, 0.54, 20*time.Hour)
	b = claudeQuotaAuth("b", clock.now, 0.05, 3*time.Hour, 0.38, 76*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "b" {
		t.Fatalf("second pick = %q, want b", got)
	}
}

func TestQuotaPacing_BurstSpreadsAcrossNearEqualAccounts(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	a := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.40, 50*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.11, 4*time.Hour, 0.41, 51*time.Hour)
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		counts[mustPick(t, selector, sessionOpts(fmt.Sprintf("s%d", i)), a, b)]++
	}
	if counts["a"] < 3 || counts["b"] < 3 {
		t.Fatalf("burst distribution = %v, want new sessions spread across both", counts)
	}
}

func TestQuotaPacing_BurstSpillsOverBeforeShortWindowRunsOut(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	// A is far more urgent but its 5h window is already 60% used.
	a := claudeQuotaAuth("a", quotaTestNow, 0.60, 30*time.Minute, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.05, 4*time.Hour, 0.20, 140*time.Hour)
	counts := map[string]int{}
	for i := 0; i < 12; i++ {
		counts[mustPick(t, selector, sessionOpts(fmt.Sprintf("s%d", i)), a, b)]++
	}
	if counts["a"] == 0 || counts["b"] == 0 || counts["a"] > 9 {
		t.Fatalf("burst distribution = %v, want A first then spill-over to B", counts)
	}
}

func TestQuotaPacing_ResetRolloverRestoresCapacity(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	observed := quotaTestNow.Add(-3 * time.Hour)
	// A's last response showed its 5h window rejected, but that window reset an hour ago.
	a := claudeQuotaAuth("a", observed, 1, 2*time.Hour, 0.50, 21*time.Hour)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "a" {
		t.Fatalf("pick = %q, want a (its rejected window has reset)", got)
	}
}

func TestQuotaPacing_RejectedAccountsRankLastButStillServe(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	a := claudeQuotaAuth("a", quotaTestNow, 1, 3*time.Hour, 0.50, 21*time.Hour)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	b := claudeQuotaAuth("b", quotaTestNow, 1, time.Hour, 0.38, 77*time.Hour)
	b.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	c := claudeQuotaAuth("c", quotaTestNow, 0.97, 2*time.Hour, 0.50, 30*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b, c); got != "c" {
		t.Fatalf("pick = %q, want c (nearly full beats rejected)", got)
	}
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "b" {
		t.Fatalf("pick = %q, want b (earliest recovery)", got)
	}
}

func TestQuotaPacing_ProbesAccountsWithoutDataWithoutStarvingOthers(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	known := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.40, 50*time.Hour)
	fresh := &Auth{ID: "b", Provider: "claude"}
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, known, fresh); got != "b" {
		t.Fatalf("first pick = %q, want the unobserved account probed", got)
	}
	// Its probe returned nothing usable: the account with data takes new work meanwhile.
	for i := 0; i < 3; i++ {
		if got := mustPick(t, selector, cliproxyexecutor.Options{}, known, fresh); got != "a" {
			t.Fatalf("pick %d = %q, want a", i, got)
		}
	}
	clock.now = quotaTestNow.Add(quotaPacingProbeInterval)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, known, fresh); got != "b" {
		t.Fatalf("pick after probe interval = %q, want b re-probed", got)
	}
}

func TestQuotaPacing_KeepsLastKnownWindowsWhenSnapshotLacksThem(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	a := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	mustPick(t, selector, cliproxyexecutor.Options{}, a, b)
	// A later response carried only Retry-After, which replaced A's snapshot.
	a.Quota = QuotaState{ObservedAt: quotaTestNow.Add(time.Minute), Signals: map[string]string{"Retry-After": "5"}}
	clock.now = quotaTestNow.Add(2 * time.Minute)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, a, b); got != "a" {
		t.Fatalf("pick = %q, want a from its last known windows", got)
	}
}

func TestQuotaPacing_APIKeysRankAfterSubscriptions(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := newQuotaTestSelector(clock)
	apiKey := &Auth{ID: "0-api", Provider: "claude", Attributes: map[string]string{"api_key": "sk-test"}}
	pressured := claudeQuotaAuth("a", quotaTestNow, 0.90, 4*time.Hour, 0.50, 21*time.Hour)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, apiKey, pressured); got != "a" {
		t.Fatalf("pick = %q, want the subscription", got)
	}
	clock.now = quotaTestNow.Add(time.Minute)
	rejected := claudeQuotaAuth("a", clock.now, 1, 4*time.Hour, 0.50, 21*time.Hour)
	rejected.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, apiKey, rejected); got != "0-api" {
		t.Fatalf("pick = %q, want the API key over a rejected subscription", got)
	}
}

func TestQuotaPacing_NoSignalsBehavesLikeRoundRobin(t *testing.T) {
	t.Parallel()
	selector := newQuotaTestSelector(&quotaTestClock{now: quotaTestNow})
	auths := []*Auth{{ID: "c", Provider: "gemini"}, {ID: "a", Provider: "gemini"}, {ID: "b", Provider: "gemini"}}
	var got []string
	for i := 0; i < 4; i++ {
		picked, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		got = append(got, picked.ID)
	}
	if fmt.Sprint(got) != "[a b c a]" {
		t.Fatalf("picks = %v, want [a b c a]", got)
	}
}

func TestQuotaPacing_MixedProviderRoute(t *testing.T) {
	t.Parallel()
	selector := newQuotaTestSelector(&quotaTestClock{now: quotaTestNow})
	a := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	got, err := selector.Pick(context.Background(), "mixed", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{b, a})
	if err != nil || got.ID != "a" {
		t.Fatalf("Pick(mixed) = %v, %v; want a", got, err)
	}
}

func TestQuotaPacing_SessionAffinityKeepsBoundSessions(t *testing.T) {
	t.Parallel()
	clock := &quotaTestClock{now: quotaTestNow}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: newQuotaTestSelector(clock),
		TTL:      time.Hour,
	})
	defer selector.Stop()

	a := claudeQuotaAuth("a", quotaTestNow, 0.10, 4*time.Hour, 0.50, 21*time.Hour)
	b := claudeQuotaAuth("b", quotaTestNow, 0.10, 4*time.Hour, 0.38, 77*time.Hour)
	session := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_quota-pacing-bound"}}`)}
	if got := mustPick(t, selector, session, a, b); got != "a" {
		t.Fatalf("new session = %q, want a", got)
	}
	// A turns pressured: the bound session stays, a new session goes to B.
	clock.now = quotaTestNow.Add(time.Minute)
	a = claudeQuotaAuth("a", clock.now, 0.96, 4*time.Hour, 0.52, 21*time.Hour)
	if got := mustPick(t, selector, session, a, b); got != "a" {
		t.Fatalf("bound session = %q, want it kept on a", got)
	}
	other := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_x_account__session_quota-pacing-new"}}`)}
	if got := mustPick(t, selector, other, a, b); got != "b" {
		t.Fatalf("second session = %q, want b", got)
	}
	// A becomes unavailable: the bound session fails over to B.
	if got := mustPick(t, selector, session, b); got != "b" {
		t.Fatalf("failover = %q, want b", got)
	}
}
