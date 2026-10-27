package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// QuotaPacingSelector routes new work to the subscription credential whose quota is most at
// risk of expiring unused, and steers new work away from credentials whose short (5-hour)
// window is on course to run out before it resets.
//
// It reads the passive rate-limit snapshot the host already keeps in Auth.Quota.Signals
// (Anthropic unified 5h/7d windows, Codex primary/secondary windows) and never sends requests
// of its own. It only ranks the candidates it is given: every credential the host offers stays
// selectable, so a pick never fails where round-robin would have returned a credential.
//
// Under session affinity it is only consulted for unbound sessions, failover rebinds and
// requests without a session, so bound conversations keep their prompt cache.
type QuotaPacingSelector struct {
	mu         sync.Mutex
	lastPicked map[string]string
	strides    map[string]map[string]float64
	states     map[string]*quotaPacingState

	// now overrides the clock in tests.
	now func() time.Time
}

const (
	// quotaPacingLongWindowMin separates short windows (5h) from long ones (weekly).
	quotaPacingLongWindowMin = 24 * time.Hour
	// quotaPacingUrgencyHorizonMin bounds urgency for windows about to reset.
	quotaPacingUrgencyHorizonMin = time.Hour
	// quotaPacingUrgencyBand keeps pressured credentials within this fraction of the best
	// urgency in contention.
	quotaPacingUrgencyBand = 0.10
	// quotaPacingUrgencyFloor keeps a credential whose remaining week is already covered by its
	// bound sessions in the weighted rotation with a small share.
	quotaPacingUrgencyFloor = 0.0005
	// quotaPacingHeadroomFloor is the smallest short-window headroom used as a weight factor.
	quotaPacingHeadroomFloor = 0.05
	// quotaPacingShortLoadTie treats short-window loads this close as equal.
	quotaPacingShortLoadTie = 0.02
	// quotaPacingAdmissionLoad is the short-window share one newly admitted session is assumed
	// to consume before its own responses show up in the observed utilization. It spreads a
	// burst of new sessions instead of sending all of them to one credential.
	quotaPacingAdmissionLoad = 0.05
	quotaPacingAdmissionTTL  = 15 * time.Minute
	// A credential is pressured when its short window is projected to run out before reset,
	// is nearly used, or its long window is nearly used.
	quotaPacingShortNearCap = 0.95
	quotaPacingLongNearCap  = 0.95
	// Short-window burn rate: slope between observations of the same window, or the average
	// since the window opened when the history is too short.
	quotaPacingShortRateLookback = 45 * time.Minute
	quotaPacingShortRateMinSpan  = 3 * time.Minute
	quotaPacingShortElapsedFloor = 15 * time.Minute
	// Long-window burn rate of the sessions already bound to a credential.
	quotaPacingLongRateLookback = 3 * time.Hour
	quotaPacingLongRateMinSpan  = 30 * time.Minute
	// No new observation for this long means no traffic, so the burn rate is zero.
	quotaPacingIdleAfter = 15 * time.Minute
	// A credential without usable quota data is preferred once per probe interval so it
	// reports its windows, without starving credentials that already have data.
	quotaPacingProbeInterval = 10 * time.Minute
	quotaPacingMaxSamples    = 64
	quotaPacingMaxStates     = 1024
	quotaPacingStateTTL      = 48 * time.Hour
)

// Candidate tiers in preference order. The first non-empty tier supplies the pick.
const (
	quotaTierProbe     = iota // capable credential with no quota data, not probed recently
	quotaTierHealthy          // room in every window
	quotaTierUnknown          // capable credential with no quota data, probed recently
	quotaTierPressured        // short window projected to run out, or a window nearly used
	quotaTierNoSignals        // credential that reports no subscription windows (API keys, other providers)
	quotaTierRejected         // provider reported a rejected window
)

var quotaTierNames = [...]string{"probe", "healthy", "unknown", "pressured", "no-signals", "rejected"}

type quotaWindow struct {
	utilization float64 // fraction used, 0..1
	resetAt     time.Time
	length      time.Duration
	rejected    bool
	rolled      bool // the observed window already reset; utilization restarts at zero
}

type quotaWindows struct {
	observedAt   time.Time
	short        *quotaWindow
	long         *quotaWindow
	limitReached bool
}

type quotaSample struct {
	at          time.Time
	utilization float64
	resetAt     time.Time
}

type quotaPacingState struct {
	windows      *quotaWindows
	shortSamples []quotaSample
	longSamples  []quotaSample
	admissions   []time.Time
	lastProbe    time.Time
	touched      time.Time
}

type quotaPacingCandidate struct {
	auth      *Auth
	tier      int
	urgency   float64 // extra burn per hour the long window needs to finish by its reset
	shortLoad float64 // projected short-window use at its reset, plus recent admissions
	recoverAt time.Time
	windows   *quotaWindows
}

// Pick selects the credential that should take new work.
func (s *QuotaPacingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*quotaPacingState)
	}
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]string)
	}

	candidates := make([]quotaPacingCandidate, 0, len(available))
	bestTier := quotaTierRejected
	for _, auth := range available {
		candidate := s.classifyLocked(auth, now)
		if candidate.tier < bestTier {
			bestTier = candidate.tier
		}
		candidates = append(candidates, candidate)
	}
	tier := make([]quotaPacingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.tier == bestTier {
			tier = append(tier, candidate)
		}
	}

	picked := s.pickWithinTierLocked(provider+":"+canonicalModelKey(model)+":"+quotaTierNames[bestTier], bestTier, tier)
	state := s.stateLocked(picked.auth.ID, now)
	if bestTier == quotaTierProbe {
		state.lastProbe = now
	}
	if sessionMetadataString(opts.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey) != "" {
		state.admissions = append(pruneQuotaAdmissions(state.admissions, now), now)
	}
	s.pruneStatesLocked(now)

	entry := selectorLogEntry(ctx).WithField("tier", quotaTierNames[bestTier])
	if picked.windows != nil {
		if picked.windows.short != nil {
			entry = entry.WithField("short_used", round2(picked.windows.short.utilization))
		}
		if picked.windows.long != nil {
			entry = entry.WithFields(map[string]any{
				"long_used":     round2(picked.windows.long.utilization),
				"long_reset_in": picked.windows.long.resetAt.Sub(now).Round(time.Minute).String(),
			})
		}
		if picked.tier == quotaTierHealthy || picked.tier == quotaTierPressured {
			entry = entry.WithFields(map[string]any{
				"urgency_per_hour": math.Round(picked.urgency*10000) / 10000,
				"short_projected":  round2(picked.shortLoad),
			})
		}
	}
	entry.Infof("quota-pacing: picked auth=%s provider=%s model=%s candidates=%d", picked.auth.ID, provider, model, len(available))
	return picked.auth, nil
}

func (s *QuotaPacingSelector) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *QuotaPacingSelector) stateLocked(authID string, now time.Time) *quotaPacingState {
	state := s.states[authID]
	if state == nil {
		state = &quotaPacingState{}
		s.states[authID] = state
	}
	state.touched = now
	return state
}

func (s *QuotaPacingSelector) pruneStatesLocked(now time.Time) {
	if len(s.states) <= quotaPacingMaxStates {
		return
	}
	for id, state := range s.states {
		if now.Sub(state.touched) > quotaPacingStateTTL {
			delete(s.states, id)
		}
	}
	if len(s.states) > quotaPacingMaxStates {
		s.states = make(map[string]*quotaPacingState)
	}
}

// classifyLocked records the credential's latest quota snapshot and places it in a tier.
func (s *QuotaPacingSelector) classifyLocked(auth *Auth, now time.Time) quotaPacingCandidate {
	candidate := quotaPacingCandidate{auth: auth}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if !quotaPacingCapable(auth, provider) {
		candidate.tier = quotaTierNoSignals
		return candidate
	}

	state := s.stateLocked(auth.ID, now)
	if observed := parseQuotaWindows(provider, auth.Quota.Signals, auth.Quota.ObservedAt); observed != nil {
		if state.windows == nil || observed.observedAt.After(state.windows.observedAt) {
			state.windows = observed
			state.shortSamples = appendQuotaSample(state.shortSamples, observed.short, observed.observedAt)
			state.longSamples = appendQuotaSample(state.longSamples, observed.long, observed.observedAt)
		}
	}
	if state.windows == nil {
		if now.Sub(state.lastProbe) >= quotaPacingProbeInterval {
			candidate.tier = quotaTierProbe
		} else {
			candidate.tier = quotaTierUnknown
		}
		return candidate
	}

	windows := state.windows.at(now)
	candidate.windows = windows
	if recoverAt, rejected := windows.rejection(); rejected {
		candidate.tier = quotaTierRejected
		candidate.recoverAt = recoverAt
		return candidate
	}

	state.admissions = pruneQuotaAdmissions(state.admissions, now)
	candidate.shortLoad = float64(len(state.admissions)) * quotaPacingAdmissionLoad
	if windows.short != nil {
		rate := shortWindowRate(state.shortSamples, windows.short, windows.observedAt, now)
		candidate.shortLoad += windows.short.utilization + rate*hoursUntil(windows.short.resetAt, now)
	}

	urgencyWindow := windows.long
	if urgencyWindow == nil {
		urgencyWindow = windows.short
	}
	horizon := math.Max(hoursUntil(urgencyWindow.resetAt, now), quotaPacingUrgencyHorizonMin.Hours())
	candidate.urgency = (1 - urgencyWindow.utilization) / horizon
	if windows.long != nil {
		candidate.urgency -= longWindowRate(state.longSamples, windows.long, windows.observedAt, now)
	}

	pressured := candidate.shortLoad >= 1 ||
		(windows.short != nil && windows.short.utilization >= quotaPacingShortNearCap) ||
		(windows.long != nil && windows.long.utilization >= quotaPacingLongNearCap)
	if pressured {
		candidate.tier = quotaTierPressured
	} else {
		candidate.tier = quotaTierHealthy
	}
	return candidate
}

// pickWithinTierLocked chooses among candidates that share the best tier.
func (s *QuotaPacingSelector) pickWithinTierLocked(rotationKey string, tier int, candidates []quotaPacingCandidate) quotaPacingCandidate {
	if len(candidates) == 1 {
		s.lastPicked[rotationKey] = candidates[0].auth.ID
		return candidates[0]
	}
	contenders := candidates
	switch tier {
	case quotaTierHealthy:
		return s.pickWeightedLocked(rotationKey, contenders)
	case quotaTierPressured:
		// Every option is short on room: keep failovers rare by taking the most headroom.
		contenders = filterQuotaCandidates(contenders, lowestShortLoad(contenders))
		contenders = filterQuotaCandidates(contenders, urgencyBand(contenders))
	case quotaTierRejected:
		earliest := contenders[0].recoverAt
		for _, candidate := range contenders[1:] {
			if candidate.recoverAt.Before(earliest) {
				earliest = candidate.recoverAt
			}
		}
		contenders = filterQuotaCandidates(contenders, func(candidate quotaPacingCandidate) bool {
			return !candidate.recoverAt.After(earliest)
		})
	}
	// Remaining contenders are equivalent: rotate by ID so they share new work.
	ids := make([]*Auth, len(contenders))
	for i, candidate := range contenders {
		ids[i] = candidate.auth
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].ID < ids[j].ID })
	if len(s.lastPicked) >= quotaPacingMaxStates {
		s.lastPicked = make(map[string]string)
	}
	pickedID := ids[successorIndex(ids, s.lastPicked[rotationKey])].ID
	s.lastPicked[rotationKey] = pickedID
	for _, candidate := range contenders {
		if candidate.auth.ID == pickedID {
			return candidate
		}
	}
	return contenders[0]
}

// pickWeightedLocked shares new work among healthy credentials by smooth weighted rotation.
// The weight is weekly urgency times short-window headroom: the credential most at risk of
// losing weekly quota takes most new sessions, but a burst that arrives before any response
// updates the snapshot is spread instead of piled onto one short window.
func (s *QuotaPacingSelector) pickWeightedLocked(rotationKey string, candidates []quotaPacingCandidate) quotaPacingCandidate {
	if s.strides == nil || len(s.strides) >= quotaPacingMaxStates {
		s.strides = make(map[string]map[string]float64)
	}
	current := s.strides[rotationKey]
	if current == nil {
		current = make(map[string]float64, len(candidates))
		s.strides[rotationKey] = current
	}
	present := make(map[string]struct{}, len(candidates))
	total := 0.0
	for _, candidate := range candidates {
		weight := (math.Max(candidate.urgency, 0) + quotaPacingUrgencyFloor) *
			math.Max(1-candidate.shortLoad, quotaPacingHeadroomFloor)
		current[candidate.auth.ID] += weight
		total += weight
		present[candidate.auth.ID] = struct{}{}
	}
	for id := range current {
		if _, ok := present[id]; !ok {
			delete(current, id)
		}
	}
	best := 0
	for i, candidate := range candidates[1:] {
		bestID := candidates[best].auth.ID
		if current[candidate.auth.ID] > current[bestID] ||
			(current[candidate.auth.ID] == current[bestID] && candidate.auth.ID < bestID) {
			best = i + 1
		}
	}
	current[candidates[best].auth.ID] -= total
	s.lastPicked[rotationKey] = candidates[best].auth.ID
	return candidates[best]
}

func filterQuotaCandidates(candidates []quotaPacingCandidate, keep func(quotaPacingCandidate) bool) []quotaPacingCandidate {
	out := make([]quotaPacingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if keep(candidate) {
			out = append(out, candidate)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

func urgencyBand(candidates []quotaPacingCandidate) func(quotaPacingCandidate) bool {
	best := math.Inf(-1)
	for _, candidate := range candidates {
		best = math.Max(best, candidate.urgency)
	}
	floor := best - quotaPacingUrgencyBand*math.Abs(best)
	return func(candidate quotaPacingCandidate) bool { return candidate.urgency >= floor }
}

func lowestShortLoad(candidates []quotaPacingCandidate) func(quotaPacingCandidate) bool {
	lowest := math.Inf(1)
	for _, candidate := range candidates {
		lowest = math.Min(lowest, candidate.shortLoad)
	}
	return func(candidate quotaPacingCandidate) bool {
		return candidate.shortLoad <= lowest+quotaPacingShortLoadTie
	}
}

// quotaPacingCapable reports whether the credential is a subscription that reports usage windows.
func quotaPacingCapable(auth *Auth, provider string) bool {
	if provider != "claude" && provider != "codex" {
		return false
	}
	return auth.AuthKind() != AuthKindAPIKey
}

// at returns the windows as they stand at now: a window whose reset has passed starts over.
func (w *quotaWindows) at(now time.Time) *quotaWindows {
	out := &quotaWindows{observedAt: w.observedAt, limitReached: w.limitReached}
	out.short = w.short.at(now)
	out.long = w.long.at(now)
	if out.limitReached && (out.short == nil || out.short.rolled) && (out.long == nil || out.long.rolled) {
		out.limitReached = false
	}
	return out
}

func (w *quotaWindow) at(now time.Time) *quotaWindow {
	if w == nil {
		return nil
	}
	copyWindow := *w
	if !copyWindow.resetAt.After(now) {
		periods := now.Sub(copyWindow.resetAt)/copyWindow.length + 1
		copyWindow.resetAt = copyWindow.resetAt.Add(periods * copyWindow.length)
		copyWindow.utilization = 0
		copyWindow.rejected = false
		copyWindow.rolled = true
	}
	return &copyWindow
}

// rejection reports whether the provider refused a window, and when the credential recovers:
// the latest reset among the windows that block it.
func (w *quotaWindows) rejection() (time.Time, bool) {
	var recoverAt time.Time
	rejected := false
	for _, window := range []*quotaWindow{w.short, w.long} {
		if window != nil && window.rejected {
			rejected = true
			if window.resetAt.After(recoverAt) {
				recoverAt = window.resetAt
			}
		}
	}
	if w.limitReached && !rejected {
		rejected = true
		for _, window := range []*quotaWindow{w.short, w.long} {
			if window != nil && !window.rolled && window.resetAt.After(recoverAt) {
				recoverAt = window.resetAt
			}
		}
	}
	return recoverAt, rejected
}

// shortWindowRate estimates the short window's current burn per hour.
func shortWindowRate(samples []quotaSample, window *quotaWindow, observedAt, now time.Time) float64 {
	if window.rolled || now.Sub(observedAt) >= quotaPacingIdleAfter {
		return 0
	}
	if rate, ok := sampleSlope(samples, window.resetAt, quotaPacingShortRateLookback, quotaPacingShortRateMinSpan); ok {
		return rate
	}
	// Too little history: use the average since the window opened, with a floor on the
	// elapsed time so a reading taken moments after the window opened is not extrapolated
	// to absurd rates.
	elapsed := window.length - window.resetAt.Sub(observedAt)
	if elapsed < quotaPacingShortElapsedFloor {
		elapsed = quotaPacingShortElapsedFloor
	}
	return window.utilization / elapsed.Hours()
}

// longWindowRate estimates how fast sessions already on the credential are using its long
// window. It is zero without enough recent history.
func longWindowRate(samples []quotaSample, window *quotaWindow, observedAt, now time.Time) float64 {
	if window.rolled || now.Sub(observedAt) >= quotaPacingIdleAfter {
		return 0
	}
	rate, _ := sampleSlope(samples, window.resetAt, quotaPacingLongRateLookback, quotaPacingLongRateMinSpan)
	return rate
}

// sampleSlope returns the utilization change per hour between the latest sample and the oldest
// sample of the same window within lookback.
func sampleSlope(samples []quotaSample, resetAt time.Time, lookback, minSpan time.Duration) (float64, bool) {
	if len(samples) < 2 {
		return 0, false
	}
	latest := samples[len(samples)-1]
	if !sameQuotaReset(latest.resetAt, resetAt) {
		return 0, false
	}
	for _, sample := range samples[:len(samples)-1] {
		span := latest.at.Sub(sample.at)
		if span > lookback || !sameQuotaReset(sample.resetAt, resetAt) {
			continue
		}
		if span < minSpan {
			return 0, false
		}
		return math.Max(0, (latest.utilization-sample.utilization)/span.Hours()), true
	}
	return 0, false
}

func sameQuotaReset(a, b time.Time) bool {
	diff := a.Sub(b)
	return diff > -2*time.Minute && diff < 2*time.Minute
}

func appendQuotaSample(samples []quotaSample, window *quotaWindow, at time.Time) []quotaSample {
	if window == nil {
		return samples
	}
	samples = append(samples, quotaSample{at: at, utilization: window.utilization, resetAt: window.resetAt})
	cutoff := at.Add(-quotaPacingLongRateLookback)
	drop := 0
	for drop < len(samples)-1 && samples[drop].at.Before(cutoff) {
		drop++
	}
	if len(samples)-drop > quotaPacingMaxSamples {
		drop = len(samples) - quotaPacingMaxSamples
	}
	return append(samples[:0], samples[drop:]...)
}

func pruneQuotaAdmissions(admissions []time.Time, now time.Time) []time.Time {
	keep := admissions[:0]
	for _, at := range admissions {
		if now.Sub(at) < quotaPacingAdmissionTTL {
			keep = append(keep, at)
		}
	}
	if len(keep) > quotaPacingMaxSamples {
		keep = keep[len(keep)-quotaPacingMaxSamples:]
	}
	return keep
}

func hoursUntil(at, now time.Time) float64 {
	return math.Max(0, at.Sub(now).Hours())
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

// parseQuotaWindows reads the provider's usage windows from a passive quota snapshot. It
// returns nil when the snapshot carries no usable window.
func parseQuotaWindows(provider string, signals map[string]string, observedAt time.Time) *quotaWindows {
	if len(signals) == 0 || observedAt.IsZero() {
		return nil
	}
	var windows []*quotaWindow
	limitReached := false
	switch provider {
	case "claude":
		for _, spec := range []struct {
			name   string
			length time.Duration
		}{{"5h", 5 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
			prefix := "Anthropic-Ratelimit-Unified-" + spec.name + "-"
			status := strings.ToLower(quotaSignal(signals, prefix+"Status"))
			resetAt, okReset := parseQuotaUnixTime(quotaSignal(signals, prefix+"Reset"))
			utilization, okUtil := parseQuotaFraction(quotaSignal(signals, prefix+"Utilization"), 1)
			if status == "rejected" && !okUtil {
				utilization, okUtil = 1, true
			}
			if !okReset || !okUtil {
				continue
			}
			windows = append(windows, &quotaWindow{utilization: utilization, resetAt: resetAt, length: spec.length, rejected: status == "rejected"})
		}
	case "codex":
		for _, name := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + name + "-"
			minutes, errMinutes := strconv.ParseFloat(quotaSignal(signals, prefix+"Window-Minutes"), 64)
			if errMinutes != nil || math.IsNaN(minutes) || minutes <= 0 || minutes > 366*24*60 {
				continue
			}
			utilization, okUtil := parseQuotaFraction(quotaSignal(signals, prefix+"Used-Percent"), 100)
			resetAt, okReset := parseQuotaUnixTime(quotaSignal(signals, prefix+"Reset-At"))
			if !okReset {
				after, errAfter := strconv.ParseFloat(quotaSignal(signals, prefix+"Reset-After-Seconds"), 64)
				if errAfter == nil && !math.IsNaN(after) && !math.IsInf(after, 0) && after >= 0 && after <= 366*24*3600 {
					resetAt, okReset = observedAt.Add(time.Duration(after*float64(time.Second))), true
				}
			}
			if !okUtil || !okReset {
				continue
			}
			windows = append(windows, &quotaWindow{
				utilization: utilization,
				resetAt:     resetAt,
				length:      time.Duration(minutes * float64(time.Minute)),
				rejected:    utilization >= 1,
			})
		}
		limitReached = strings.EqualFold(quotaSignal(signals, "X-Codex-Limit-Reached"), "true") ||
			strings.EqualFold(quotaSignal(signals, "X-Codex-Allowed"), "false")
	default:
		return nil
	}
	if len(windows) == 0 {
		return nil
	}
	out := &quotaWindows{observedAt: observedAt, limitReached: limitReached}
	for _, window := range windows {
		if window.length < quotaPacingLongWindowMin {
			// The most used short window is the one that runs out first.
			if out.short == nil || window.utilization > out.short.utilization {
				out.short = window
			}
			continue
		}
		// The longest window decides when unused quota is lost.
		if out.long == nil || window.length > out.long.length ||
			(window.length == out.long.length && window.utilization > out.long.utilization) {
			out.long = window
		}
	}
	return out
}

func quotaSignal(signals map[string]string, name string) string {
	if value, ok := signals[http.CanonicalHeaderKey(name)]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// parseQuotaFraction parses a utilization reported on the given scale (1 or 100) into 0..1.
func parseQuotaFraction(raw string, scale float64) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return math.Min(value/scale, 1), true
}

// parseQuotaUnixTime parses a Unix timestamp in seconds, or in milliseconds when it is too
// large to be seconds.
func parseQuotaUnixTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}, false
	}
	if value > 1e12 {
		value /= 1000
	}
	// Accept 2001-09-09 .. 2286-11-20 only, which rules out durations and garbage.
	if value < 1e9 || value > 1e10 {
		return time.Time{}, false
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(fraction*1e9)), true
}
