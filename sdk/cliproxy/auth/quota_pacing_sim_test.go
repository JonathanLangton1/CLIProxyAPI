//go:build quotasim

// Simulation of routing strategies over a pool of subscription accounts.
//
//	go test -tags quotasim -run TestQuotaPacingSimulation -v ./sdk/cliproxy/auth/
//
// Each account has a weekly window and a 5-hour window (opened by its first use, like
// Anthropic's). Sessions arrive, stick to the account they were given, and use a fixed amount
// of quota per minute. When an account hits a cap, every session on it fails over to another
// account and pays a prompt-cache rebuild there. Selectors see only what the proxy sees: the
// rounded rate-limit headers of each account's latest response.
package auth

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	simWeek    = 7 * 24 * time.Hour
	simFiveH   = 5 * time.Hour
	simHorizon = 14 * 24 * time.Hour
	simWeekCap = 1000.0
)

var simStart = time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC) // a Tuesday afternoon

type simAccount struct {
	id          string
	capW, cap5  float64
	usedW       float64
	used5       float64
	weekResetAt time.Time
	win5Start   time.Time
	auth        *Auth
	wasted      float64
	capacity    float64 // quota that became available inside the horizon
	served      bool
}

func (a *simAccount) roll(now time.Time) {
	for !now.Before(a.weekResetAt) {
		a.wasted += math.Max(0, a.capW-a.usedW)
		a.usedW = 0
		a.weekResetAt = a.weekResetAt.Add(simWeek)
		if now.Before(simStart.Add(simHorizon)) {
			a.capacity += a.capW
		}
	}
	if !a.win5Start.IsZero() && !now.Before(a.win5Start.Add(simFiveH)) {
		a.win5Start = time.Time{}
		a.used5 = 0
	}
}

func (a *simAccount) room() float64 { return math.Min(a.capW-a.usedW, a.cap5-a.used5) }

func (a *simAccount) available() bool { return a.room() > 1e-9 }

func (a *simAccount) consume(now time.Time, units float64) float64 {
	if a.win5Start.IsZero() {
		a.win5Start = now
	}
	served := math.Min(units, math.Max(0, a.room()))
	a.usedW += served
	a.used5 += served
	a.served = true
	return served
}

// observe writes the headers the account's latest response would carry.
func (a *simAccount) observe(now time.Time) {
	reset5 := now.Add(simFiveH)
	if !a.win5Start.IsZero() {
		reset5 = a.win5Start.Add(simFiveH)
	}
	status := func(used, limit float64) string {
		if used >= limit-1e-9 {
			return "rejected"
		}
		return "allowed"
	}
	a.auth.Quota = QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(math.Round(a.used5/a.cap5*100)/100, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(reset5.Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Status":      status(a.used5, a.cap5),
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(math.Round(a.usedW/a.capW*100)/100, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(a.weekResetAt.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Status":      status(a.usedW, a.capW),
	}}
}

type simSession struct {
	start, end time.Time
	burn       float64
	account    *simAccount
}

// simAccountSetup is an account's state at the start: weekly use, time to weekly reset,
// 5-hour use and time to 5-hour reset (0 means no open window).
type simAccountSetup struct {
	usedW   float64
	resetIn time.Duration
	used5   float64
	reset5  time.Duration
}

type simPool struct {
	name     string
	accounts []simAccountSetup
}

type simDemand struct {
	name string
	// concurrency returns the target number of concurrent sessions at a time of day.
	concurrency func(t time.Time) float64
	// burst returns sessions to start at once at this minute, on top of the steady flow.
	burst       func(t time.Time) int
	minDuration time.Duration
	maxDuration time.Duration
	// oneShotPerHour adds tiny one-request sessions (titles, summaries, quick questions).
	oneShotPerHour float64
}

type simStrategy struct {
	name string
	new  func(clock func() time.Time) Selector
	// oracle serves each minute's total demand fluidly, earliest weekly reset first, with no
	// session stickiness and no cache cost. It bounds what any router can serve.
	oracle bool
}

type simResult struct {
	demand, served, blocked float64
	wasted, capacity        float64
	failovers, massEvents   int
	rebuild                 float64
}

// soonestResetSelector is the naive reset-aware strategy: earliest weekly reset first.
type soonestResetSelector struct{ rr RoundRobinSelector }

func (s *soonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	var best []*Auth
	var bestReset time.Time
	for _, auth := range auths {
		windows := parseQuotaWindows("claude", auth.Quota.Signals, auth.Quota.ObservedAt)
		if windows == nil || windows.long == nil {
			continue
		}
		reset := windows.long.resetAt
		switch {
		case best == nil || reset.Before(bestReset.Add(-time.Minute)):
			best, bestReset = []*Auth{auth}, reset
		case !reset.After(bestReset.Add(time.Minute)):
			best = append(best, auth)
		}
	}
	if best == nil {
		best = auths
	}
	return s.rr.Pick(ctx, provider, model, opts, best)
}

const (
	simBurnMean        = 0.08 // quota units per session-minute (week cap is 1000)
	simRebuildBase     = 0.6  // cache rebuild cost of a failover, in units...
	simRebuildPerHour  = 0.9  // ...growing with the session's age
	simMassWindow      = 10 * time.Minute
	simMassFailovers   = 3
	simMinutesPerRound = time.Minute
)

func runQuotaSim(t *testing.T, pool simPool, demand simDemand, strategy simStrategy, weekTo5h float64, seed int64) simResult {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	now := simStart
	clock := func() time.Time { return now }
	var selector Selector
	if !strategy.oracle {
		selector = strategy.new(clock)
	}

	accounts := make([]*simAccount, len(pool.accounts))
	for i, setup := range pool.accounts {
		a := &simAccount{
			id:          fmt.Sprintf("acct-%c", 'a'+i),
			capW:        simWeekCap,
			cap5:        simWeekCap / weekTo5h,
			usedW:       setup.usedW * simWeekCap,
			weekResetAt: simStart.Add(setup.resetIn),
		}
		a.capacity = a.capW - a.usedW
		if setup.reset5 > 0 {
			a.used5 = setup.used5 * a.cap5
			a.win5Start = simStart.Add(setup.reset5 - simFiveH)
		}
		a.auth = &Auth{ID: a.id, Provider: "claude"}
		a.observe(now)
		accounts[i] = a
	}

	var res simResult
	var sessions []*simSession
	var failoverTimes []time.Time
	lastMass := time.Time{}
	avgDuration := (demand.minDuration + demand.maxDuration) / 2
	nextSession := 0
	ctx := context.Background()

	pick := func(exclude *simAccount) *simAccount {
		var candidates []*Auth
		byID := map[string]*simAccount{}
		for _, a := range accounts {
			if a.available() && a != exclude {
				candidates = append(candidates, a.auth)
				byID[a.id] = a
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		nextSession++
		opts := cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.SessionAffinityProviderMetadataKey: "claude",
			cliproxyexecutor.CanonicalSessionIDMetadataKey:      strconv.Itoa(nextSession),
		}}
		picked, err := selector.Pick(ctx, "claude", "claude-opus-5-5", opts, candidates)
		if err != nil || picked == nil {
			t.Fatalf("%s: Pick() = %v, %v with %d candidates", strategy.name, picked, err, len(candidates))
		}
		return byID[picked.ID]
	}

	for now.Before(simStart.Add(simHorizon)) {
		for _, a := range accounts {
			a.roll(now)
		}
		// Arrivals: Poisson flow toward the target concurrency, plus bursts.
		rate := demand.concurrency(now) / avgDuration.Minutes()
		arrivals := poisson(rng, rate)
		if demand.burst != nil {
			arrivals += demand.burst(now)
		}
		oneShots := poisson(rng, demand.oneShotPerHour/60)
		if demand.concurrency(now) <= 0 {
			oneShots = 0
		}
		for i := oneShots; i > 0; i-- {
			sessions = append(sessions, &simSession{start: now, end: now.Add(time.Minute), burn: 0.01})
		}
		for i := 0; i < arrivals; i++ {
			duration := demand.minDuration + time.Duration(rng.Int63n(int64(demand.maxDuration-demand.minDuration)+1))
			burn := simBurnMean * (0.5 + rng.Float64())
			sessions = append(sessions, &simSession{start: now, end: now.Add(duration), burn: burn})
		}

		active := sessions[:0]
		for _, s := range sessions {
			if now.Before(s.end) {
				active = append(active, s)
			}
		}
		sessions = active

		if strategy.oracle {
			need := 0.0
			for _, s := range sessions {
				need += s.burn
			}
			res.demand += need
			order := append([]*simAccount(nil), accounts...)
			sort.Slice(order, func(i, j int) bool { return order[i].weekResetAt.Before(order[j].weekResetAt) })
			for _, a := range order {
				if need <= 1e-12 || !a.available() {
					continue
				}
				served := a.consume(now, need)
				need -= served
				res.served += served
			}
			res.blocked += need
		}
		for _, s := range sessions {
			if strategy.oracle {
				break
			}
			res.demand += s.burn
			if s.account == nil || !s.account.available() {
				previous := s.account
				next := pick(nil)
				if next == nil {
					res.blocked += s.burn
					continue
				}
				if previous != nil {
					res.failovers++
					failoverTimes = append(failoverTimes, now)
					rebuild := simRebuildBase + simRebuildPerHour*now.Sub(s.start).Hours()
					res.rebuild += next.consume(now, rebuild)
				}
				s.account = next
			}
			served := s.account.consume(now, s.burn)
			res.served += served
			res.blocked += s.burn - served
		}

		// Mass failover: several sessions forced off accounts within a short window.
		cut := 0
		for cut < len(failoverTimes) && now.Sub(failoverTimes[cut]) > simMassWindow {
			cut++
		}
		failoverTimes = failoverTimes[cut:]
		if len(failoverTimes) >= simMassFailovers && now.Sub(lastMass) > simMassWindow {
			res.massEvents++
			lastMass = now
		}

		for _, a := range accounts {
			if a.served {
				a.observe(now)
				a.served = false
			}
		}
		now = now.Add(simMinutesPerRound)
	}
	for _, a := range accounts {
		// Quota still unused at the horizon is not lost yet; only completed resets count.
		res.wasted += a.wasted
		res.capacity += a.capacity
	}
	return res
}

func poisson(rng *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func workHours(from, to int, weekdaysOnly bool, level float64) func(time.Time) float64 {
	return func(t time.Time) float64 {
		if weekdaysOnly && (t.Weekday() == time.Saturday || t.Weekday() == time.Sunday) {
			return 0
		}
		if t.Hour() >= from && t.Hour() < to {
			return level
		}
		return 0
	}
}

func TestQuotaPacingSimulation(t *testing.T) {
	log.SetOutput(io.Discard)

	pools := []simPool{
		{"today (A 50%/21h, B 38%/77h)", []simAccountSetup{{0.50, 21 * time.Hour, 0.12, 4 * time.Hour}, {0.38, 77 * time.Hour, 0.10, 2 * time.Hour}}},
		{"aligned resets (both fresh)", []simAccountSetup{{0, 168 * time.Hour, 0, 0}, {0, 168 * time.Hour, 0, 0}}},
		{"A nearly spent (97%/2h)", []simAccountSetup{{0.97, 2 * time.Hour, 0.30, 3 * time.Hour}, {0.38, 77 * time.Hour, 0, 0}}},
		{"three staggered", []simAccountSetup{{0.20, 30 * time.Hour, 0, 0}, {0.60, 100 * time.Hour, 0, 0}, {0, 150 * time.Hour, 0, 0}}},
	}
	demands := []simDemand{
		{name: "light (2 sessions, weekdays 9-18)", concurrency: workHours(9, 18, true, 2), minDuration: 20 * time.Minute, maxDuration: 120 * time.Minute},
		{name: "moderate (4 sessions, 9-21 daily)", concurrency: workHours(9, 21, false, 4), minDuration: 30 * time.Minute, maxDuration: 180 * time.Minute},
		{name: "heavy (7 sessions, 8-23 daily)", concurrency: workHours(8, 23, false, 7), minDuration: 30 * time.Minute, maxDuration: 240 * time.Minute},
		{name: "bursty (8 at once every 3h, 9-21)", concurrency: workHours(9, 21, false, 1),
			burst: func(t time.Time) int {
				if t.Hour() >= 9 && t.Hour() < 21 && t.Hour()%3 == 0 && t.Minute() == 0 {
					return 8
				}
				return 0
			}, minDuration: 45 * time.Minute, maxDuration: 150 * time.Minute},
		{name: "saturated (10 sessions, 24/7)", concurrency: func(time.Time) float64 { return 10 }, minDuration: 30 * time.Minute, maxDuration: 240 * time.Minute},
	}
	for _, demand := range demands[:len(demands):len(demands)] {
		demand.name += " +50 one-shots/h"
		demand.oneShotPerHour = 50
		demands = append(demands, demand)
	}
	strategies := []simStrategy{
		{name: "ORACLE-bound", oracle: true},
		{name: "round-robin", new: func(func() time.Time) Selector { return &RoundRobinSelector{} }},
		{name: "soonest-reset", new: func(func() time.Time) Selector { return &soonestResetSelector{} }},
		{name: "quota-pacing", new: func(clock func() time.Time) Selector { return &QuotaPacingSelector{now: clock} }},
	}
	ratios := []float64{8, 14}
	const seeds = 8

	var out strings.Builder
	totals := map[string]*simResult{}
	for _, ratio := range ratios {
		fmt.Fprintf(&out, "\n######## weekly cap = %.0f x 5-hour cap ########\n", ratio)
		for _, pool := range pools {
			for _, demand := range demands {
				fmt.Fprintf(&out, "\n%s | %s\n", pool.name, demand.name)
				fmt.Fprintf(&out, "  %-14s %8s %8s %8s %9s %8s %9s %6s\n", "strategy", "served%", "vsBound", "blocked", "wasted@rs", "failover", "massFail", "rebld")
				oracleServed := 0.0
				for _, strategy := range strategies {
					var agg simResult
					for seed := int64(1); seed <= seeds; seed++ {
						r := runQuotaSim(t, pool, demand, strategy, ratio, seed)
						agg.demand += r.demand
						agg.served += r.served
						agg.blocked += r.blocked
						agg.wasted += r.wasted
						agg.capacity += r.capacity
						agg.failovers += r.failovers
						agg.massEvents += r.massEvents
						agg.rebuild += r.rebuild
					}
					n := float64(seeds)
					if strategy.oracle {
						oracleServed = agg.served
					}
					fmt.Fprintf(&out, "  %-14s %7.2f%% %7.2f%% %8.1f %9.1f %8.1f %9.1f %6.1f\n", strategy.name,
						100*agg.served/agg.demand, 100*agg.served/oracleServed, agg.blocked/n, agg.wasted/n,
						float64(agg.failovers)/n, float64(agg.massEvents)/n, agg.rebuild/n)
					total := totals[strategy.name]
					if total == nil {
						total = &simResult{}
						totals[strategy.name] = total
					}
					total.demand += agg.demand / n
					total.served += agg.served / n
					total.capacity += oracleServed / n
					total.blocked += agg.blocked / n
					total.wasted += agg.wasted / n
					total.failovers += int(math.Round(float64(agg.failovers) / n))
					total.massEvents += int(math.Round(float64(agg.massEvents) / n))
					total.rebuild += agg.rebuild / n
				}
			}
		}
	}
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(&out, "\n######## totals across all scenarios (per-run averages summed) ########\n")
	fmt.Fprintf(&out, "  %-14s %8s %8s %9s %9s %8s %9s %8s\n", "strategy", "served%", "vsBound", "blocked", "wasted@rs", "failover", "massFail", "rebld")
	for _, name := range names {
		r := totals[name]
		fmt.Fprintf(&out, "  %-14s %7.2f%% %7.2f%% %9.1f %9.1f %8d %9d %8.1f\n", name, 100*r.served/r.demand, 100*r.served/r.capacity, r.blocked, r.wasted, r.failovers, r.massEvents, r.rebuild)
	}
	t.Log(out.String())
}
