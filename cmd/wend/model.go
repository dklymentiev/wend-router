package main

import (
	"math"
	"strings"
	"sync"
	"time"
)

// capability is what the router knows about a feature of a model.
type capability int8

const (
	capUnknown capability = iota
	capYes
	capNo
)

func (c capability) String() string {
	return [...]string{"?", "yes", "no"}[c]
}

func capFrom(b *bool) capability {
	switch {
	case b == nil:
		return capUnknown
	case *b:
		return capYes
	}
	return capNo
}

const (
	breakerThreshold = 3                // consecutive failures that open the circuit
	breakerBase      = 30 * time.Second // first open period, doubles per trip
	breakerMax       = 10 * time.Minute
	historySize      = 50        // outcomes kept for the availability figure
	rpmWindow        = 120       // attempt start times kept for requests per minute
	goneFor          = time.Hour // a refused model is left alone this long
	paidOnlyFor      = 24 * time.Hour
	unmeasuredTTFT   = 3000 // ms, ranking guess for a model with no measurement
	ewmaWeight       = 0.3
)

// Model is one model at one provider, with everything learned about it.
type Model struct {
	Provider *Provider
	ID       string // the id the provider expects
	Key      string // provider/id, the name agents and the status page use
	Family   string // the model itself, whoever serves it: the same model at two providers shares it
	Tier     int

	priceIn, priceOut float64 // USD per token; zero for free models
	trains            bool    // the free tier may train on prompts sent to this model
	rep               reputation
	tags              []string    // domains from the seed and from name and description
	test              *testResult // last test run, nil when never tested
	seedTier          int         // the seed's tier; Tier is the measured one once tested

	mu          sync.Mutex
	context     int // max context in tokens, 0 when unknown
	tools       capability
	vision      capability
	reasoning   capability // the model thinks out loud before answering
	gone        bool       // the provider no longer lists it
	goneUntil   time.Time  // the provider refused it (404, model-level 403)
	fails       int        // consecutive failures
	trips       int        // breaker openings since the last success
	openUntil   time.Time
	pausedUntil time.Time // quota exhausted
	ttftMs      float64   // EWMA of time to first token
	tps         float64   // EWMA of output tokens per second after the first token
	inflight    int
	starts      [rpmWindow]time.Time // ring of recent attempt start times
	startN      int
	totalMs     float64 // EWMA of full response time
	ok, errs    int
	history     [historySize]bool
	histN       int
	lastErr     string
	lastErrAt   time.Time
	lastOK      time.Time
	lastProbe   time.Time
	windows     map[string]window // rate-limit windows the provider reported
	testPart    *testResult       // a test run cut short, finished on the next run
	maxRequest  int               // largest request in tokens the account accepts at once (a per-minute cap), 0 when none
	quotaStreak int               // quota refusals since the last answer
	capSince    time.Time         // first refusal of the current used-up allowance
	capWindow   time.Duration     // how long an allowance stayed used up last time, 0 when not seen
	dropNames   bool              // the provider rejects the name field on messages
	topP1       bool              // the provider wants top_p 1 with temperature 0 (Mistral)
}

// usable reports whether the model may take a request now. A model whose
// breaker period ran out is usable again: the next request is its trial.
func (m *Model) usable(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.gone && now.After(m.goneUntil) && now.After(m.openUntil) && now.After(m.pausedUntil) && m.Provider.usable(now)
}

// hasRoom reports whether the model's provider takes one more request now.
func (m *Model) hasRoom() bool { return m.Provider.hasRoom() }

// begin and end bracket one attempt on the model, for the load figures.
func (m *Model) begin() {
	m.Provider.inflight.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight++
	m.starts[m.startN%rpmWindow] = time.Now()
	m.startN++
}

func (m *Model) end() {
	m.Provider.inflight.Add(-1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight--
}

// minGenWindow is the shortest stretch from the first token to the last
// that says anything about speed. A provider that holds the answer and sends
// it at once (llm7) delivers 300 tokens in 0.14 s, which read as 2,000 tok/s.
const minGenWindow = 500 * time.Millisecond

// tokensPerSecond is the generation speed after the first token, 0 when the
// answer came too fast to measure.
func tokensPerSecond(ttft, total time.Duration, tokens int) float64 {
	gen := total - ttft
	if tokens <= 1 || gen < minGenWindow {
		return 0
	}
	return float64(tokens) / gen.Seconds()
}

func (m *Model) recordSuccess(ttft, total time.Duration, tokens int) {
	m.rep.add(true)
	m.Provider.rep.add(true)
	m.mu.Lock()
	defer m.mu.Unlock()
	if tps := tokensPerSecond(ttft, total, tokens); tps > 0 {
		m.tps = ewma(m.tps, tps)
	}
	m.fails, m.trips = 0, 0
	m.openUntil = time.Time{}
	m.quotaStreak = 0
	if !m.capSince.IsZero() {
		// The allowance is back: this is how long it stayed used up (at
		// most; the router only learns it when it tries again).
		m.capWindow = time.Since(m.capSince)
		m.capSince = time.Time{}
	}
	m.ok++
	m.lastOK = time.Now()
	m.ttftMs = ewma(m.ttftMs, float64(ttft.Milliseconds()))
	m.totalMs = ewma(m.totalMs, float64(total.Milliseconds()))
	m.push(true)
}

const (
	ttftFactor     = 4                      // a model gets this many times its usual wait for the first token
	ttftTrustAfter = 3                      // answers before its usual wait is trusted
	prefillPerTok  = 200 * time.Microsecond // reading the prompt: 1 s per 5,000 tokens
)

// firstTokenDeadline is how long m may take to its first token: a few times
// what it usually takes, plus time to read a long prompt, within [lo, hi].
// lo of 0 turns the adaptation off; a model with too few answers gets hi.
func (m *Model) firstTokenDeadline(lo, hi time.Duration, promptTokens int) time.Duration {
	m.mu.Lock()
	usual, ok := m.ttftMs, m.ok
	m.mu.Unlock()
	if lo <= 0 || usual == 0 || ok < ttftTrustAfter {
		return hi
	}
	d := time.Duration(usual*ttftFactor)*time.Millisecond + time.Duration(promptTokens)*prefillPerTok
	return min(max(d, lo), hi)
}

// noteSlow counts a wait the router gave up on as a slow answer, so a model
// that used to be fast drops in the speed ranking at once rather than after
// the breaker opens. It only ever makes the usual wait longer.
func (m *Model) noteSlow(waited time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ms := float64(waited.Milliseconds()); ms > m.ttftMs {
		m.ttftMs = ewma(m.ttftMs, ms)
	}
}

// recordFailure updates the model after a failed attempt. est is the
// request's estimated size in tokens, used to learn the real context limit.
func (m *Model) recordFailure(kind failKind, pause time.Duration, msg string, ri *reqInfo) time.Duration {
	if countsForReputation(kind) {
		m.rep.add(false)
		m.Provider.rep.add(false)
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == failField {
		// The model is fine; the request had a field it does not take, and
		// it goes without that field from now on.
		if strings.Contains(strings.ToLower(msg), "top_p must be 1") {
			m.topP1 = true
		} else {
			m.dropNames = true
		}
		return 0
	}
	if kind == failBusy {
		// The provider is full, not the model: remember how many it takes
		// at once and do not count this against the model.
		m.Provider.noteBusy()
		return 0
	}
	m.errs++
	m.lastErr = kind.String() + ": " + truncate(msg, 200)
	m.lastErrAt = now
	switch kind {
	case failTransient, failEmpty:
		m.push(false)
		m.fails++
		if m.fails >= breakerThreshold {
			m.openUntil = now.Add(min(breakerBase<<m.trips, breakerMax))
			m.trips++
			m.fails = 0
		}
	case failQuota:
		m.push(false)
		m.quotaStreak++
		if pause <= 0 {
			pause = m.guessPause(msg, now)
		}
		m.pausedUntil = now.Add(pause)
	case failNotFound:
		m.goneUntil = now.Add(goneFor)
	case failPaid:
		m.goneUntil = now.Add(paidOnlyFor)
	case failTooLarge:
		if limit := perMinuteCap(msg); limit > 0 {
			m.maxRequest = limit // a cap of the tier, not the context window
		} else if ri.estTokens > 0 && (m.context == 0 || ri.estTokens <= m.context) {
			m.context = ri.estTokens - 1
		}
	case failField:
		m.dropNames = true
	case failUnsupported:
		if ri.hasTools {
			m.tools = capNo
		} else if ri.hasImage {
			m.vision = capNo
		}
	}
	if kind == failQuota {
		return pause
	}
	return 0
}

// guessPause is the pause for a quota refusal that named no reset time.
// An allowance used up (a promotion, a day) waits as long as it took to
// come back last time, else firstCapPause, doubling while it stays refused.
// A rate refusal starts at a minute and doubles while refusals keep coming,
// so a model the provider has stopped is not knocked on every minute.
// m.mu is held.
func (m *Model) guessPause(msg string, now time.Time) time.Duration {
	if capRefusal(msg) {
		if m.capSince.IsZero() {
			m.capSince = now
			if m.capWindow > 0 {
				return min(m.capWindow, maxQuotaPause)
			}
			return firstCapPause
		}
		// Still refused after the pause: wait as long again as it has lasted.
		return min(max(now.Sub(m.capSince), firstCapPause), maxQuotaPause)
	}
	d := defaultQuotaPause << min(m.quotaStreak-1, 5)
	return min(d, maxBackoffPause)
}

func (m *Model) push(ok bool) {
	m.history[m.histN%historySize] = ok
	m.histN++
}

// fits reports whether the model can serve the request's needs as far as
// the router knows. Unknown capabilities are given the benefit of the doubt.
func (m *Model) fits(ri *reqInfo) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ri.hasTools && m.tools == capNo {
		return false
	}
	if ri.hasImage && m.vision == capNo {
		return false
	}
	if m.maxRequest > 0 && ri.estTokens > m.maxRequest {
		return false
	}
	return m.context == 0 || ri.estTokens <= m.context
}

// sortTTFT is the speed used for ranking. An unmeasured model gets a
// pessimistic guess: a model that has answered fast beats one that never
// answered, and unmeasured models are tried when the measured ones fail.
func (m *Model) sortTTFT() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ttftMs == 0 {
		return unmeasuredTTFT
	}
	return m.ttftMs
}

type modelStatus struct {
	Key          string            `json:"key"`
	Provider     string            `json:"provider"`
	Tier         int               `json:"tier"`
	State        string            `json:"state"`
	Context      int               `json:"context,omitempty"`
	Tools        string            `json:"tools"`
	Vision       string            `json:"vision"`
	Reasoning    string            `json:"reasoning"`
	TTFTMs       int               `json:"ttft_ms,omitempty"`
	TotalMs      int               `json:"total_ms,omitempty"`
	Availability float64           `json:"availability"` // share of recent attempts that worked, -1 when none
	OK           int               `json:"ok"`
	Errors       int               `json:"errors"`
	Until        string            `json:"until,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
	LastErrorAt  string            `json:"last_error_at,omitempty"`
	TrainsOnFree bool              `json:"trains_on_free"`
	Health       string            `json:"health"`                // ready, untested or failing: what the last attempt said
	LastTouch    string            `json:"last_touch,omitempty"`  // last request or probe, whatever its outcome
	PriceInM     float64           `json:"price_in_m,omitempty"`  // USD per million input tokens
	PriceOutM    float64           `json:"price_out_m,omitempty"` // USD per million output tokens
	TPS          float64           `json:"tps,omitempty"`
	InFlight     int               `json:"inflight"`
	RPM          int               `json:"rpm"`     // attempts started in the last minute
	Usually      string            `json:"usually"` // new, stable, mostly ok or flaky
	UsuallyShare float64           `json:"usually_share,omitempty"`
	Domains      []string          `json:"domains,omitempty"`
	Test         *testView         `json:"test,omitempty"`
	SeedTier     int               `json:"seed_tier,omitempty"`
	Limits       map[string]window `json:"limits,omitempty"`
	MaxRequest   int               `json:"max_request,omitempty"`
	DropNames    bool              `json:"drop_names,omitempty"`
	TopP1        bool              `json:"top_p_1,omitempty"`
}

func (m *Model) status(now time.Time) modelStatus {
	usually := m.rep.label()
	share, _ := m.rep.share()
	domains := m.domains()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := healthReady
	switch {
	case m.lastOK.IsZero() && m.lastErrAt.IsZero():
		h = healthUntested
	case m.lastErrAt.After(m.lastOK):
		h = healthFailing
	}
	s := modelStatus{
		Test: m.test.view(), SeedTier: m.seedTier, Limits: m.limitsView(now), MaxRequest: m.maxRequest, DropNames: m.dropNames, TopP1: m.topP1,
		Health: healthNames[h], Usually: usually, Domains: domains, UsuallyShare: math.Round(share*100) / 100,
		LastTouch: rfc3339(latest(m.lastOK, m.lastErrAt, m.lastProbe)),
		Key:       m.Key, Provider: m.Provider.Spec.ID, Tier: m.Tier, Context: m.context,
		Tools: m.tools.String(), Vision: m.vision.String(), Reasoning: m.reasoning.String(),
		TTFTMs: int(m.ttftMs), TotalMs: int(m.totalMs), OK: m.ok, Errors: m.errs,
		LastError: m.lastErr, TrainsOnFree: m.trains,
		TPS: float64(int(m.tps*10)) / 10, InFlight: m.inflight,
		PriceInM: m.priceIn * 1e6, PriceOutM: m.priceOut * 1e6,
	}
	for i := range min(m.startN, rpmWindow) {
		if now.Sub(m.starts[i]) < time.Minute {
			s.RPM++
		}
	}
	if !m.lastErrAt.IsZero() {
		s.LastErrorAt = m.lastErrAt.Format(time.RFC3339)
	}
	n := min(m.histN, historySize)
	s.Availability = -1
	if n > 0 {
		good := 0
		for i := range n {
			if m.history[i] {
				good++
			}
		}
		s.Availability = float64(good) / float64(n)
	}
	switch {
	case m.gone:
		s.State = "gone"
	case now.Before(m.goneUntil):
		s.State, s.Until = "gone", m.goneUntil.Format(time.RFC3339)
	case !m.Provider.usable(now):
		s.State = "provider-off"
	case now.Before(m.pausedUntil):
		s.State, s.Until = "quota", m.pausedUntil.Format(time.RFC3339)
	case now.Before(m.openUntil):
		s.State, s.Until = "open", m.openUntil.Format(time.RFC3339)
	default:
		s.State = "ok"
	}
	return s
}

func ewma(prev, v float64) float64 {
	if prev == 0 {
		return v
	}
	return prev*(1-ewmaWeight) + v*ewmaWeight
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sawReasoning records that the model streamed reasoning tokens.
func (m *Model) sawReasoning() {
	m.mu.Lock()
	m.reasoning = capYes
	m.mu.Unlock()
}

// health is what the last attempt on the model says, for ranking: a model
// that just answered goes before one never tried, which goes before one
// whose last attempt failed.
type health int

const (
	healthReady health = iota
	healthUntested
	healthFailing
)

var healthNames = [...]string{"ready", "untested", "failing"}

func (m *Model) health() health {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.lastOK.IsZero() && m.lastErrAt.IsZero():
		return healthUntested
	case m.lastErrAt.After(m.lastOK):
		return healthFailing
	}
	return healthReady
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
