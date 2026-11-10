package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Proactive limits: providers that say what is left in their rate-limit
// windows (Groq, SambaNova, OVH...) get read after every answer. A window
// with no requests left pauses the model until it resets, and a request
// bigger than the tokens left is sent elsewhere, instead of waiting for a
// 429 to say the same thing.

type window struct {
	Left  int       `json:"left"`
	Reset time.Time `json:"reset"`
}

// limitHeaders maps a window name to its remaining-count and reset headers.
var limitHeaders = []struct {
	name, left, reset string
	tokens            bool
}{
	{"requests", "X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests", false},
	{"requests-day", "X-Ratelimit-Remaining-Requests-Day", "X-Ratelimit-Reset-Requests-Day", false},
	{"tokens", "X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens", true},
	{"minute", "X-Ratelimit-Remaining-Minute", "Ratelimit-Reset", false},
	{"window", "X-Ratelimit-Remaining", "X-Ratelimit-Reset", false},
}

// noteLimits reads the rate-limit headers of a response.
func (m *Model) noteLimits(h http.Header) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lh := range limitHeaders {
		v := strings.TrimSpace(h.Get(lh.left))
		if v == "" {
			continue
		}
		left, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		reset := parseReset(h.Get(lh.reset))
		if reset <= 0 {
			reset = time.Minute
		}
		if m.windows == nil {
			m.windows = map[string]window{}
		}
		w := window{Left: left, Reset: now.Add(min(reset, maxQuotaPause))}
		m.windows[lh.name] = w
		if !lh.tokens && left <= 0 && w.Reset.After(m.pausedUntil) {
			m.pausedUntil = w.Reset
		}
	}
}

// tokensFit reports whether the request fits the tokens left in the
// model's current window; unknown windows fit.
func (m *Model) tokensFit(ri *reqInfo, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.windows["tokens"]
	return !ok || now.After(w.Reset) || ri.estTokens <= w.Left
}

// limitsView is what is left in each live window, for the dashboard.
func (m *Model) limitsView(now time.Time) map[string]window {
	out := map[string]window{}
	for k, w := range m.windows {
		if now.Before(w.Reset) {
			out[k] = w
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
