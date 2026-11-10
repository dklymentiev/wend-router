package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// failKind says why an attempt failed, and so what the router does next.
type failKind int

const (
	failTransient   failKind = iota // 5xx, network, timeout: count towards the breaker
	failEmpty                       // 200 with nothing in it: same as transient
	failQuota                       // rate or quota limit: pause the model until reset
	failAuth                        // bad key or no credit: disable the provider
	failNotFound                    // provider does not serve the model
	failTooLarge                    // request exceeds the model's context
	failUnsupported                 // model lacks a capability the request needs
	failPolicy                      // provider refused on content grounds
	failClient                      // the request itself is malformed
	failPaid                        // this model needs a payment method on this account
	failField                       // the provider rejects a message field (name); sent without it next time
	failBusy                        // too many requests at once to this provider: the model is fine, the provider is full
)

var failNames = map[failKind]string{
	failTransient: "transient", failEmpty: "empty", failQuota: "quota", failAuth: "auth",
	failNotFound: "not-found", failTooLarge: "too-large", failUnsupported: "unsupported",
	failPolicy: "policy", failClient: "client", failPaid: "paid-only", failField: "field",
	failBusy: "busy",
}

func (k failKind) String() string { return failNames[k] }

const (
	defaultQuotaPause = 60 * time.Second
	maxQuotaPause     = 24 * time.Hour
	maxBackoffPause   = 30 * time.Minute // repeated refusals without a reset time grow up to this
	firstCapPause     = 15 * time.Minute // an allowance used up, window not known yet
)

// classify maps a failed upstream response to a failKind. For quota failures
// it also returns how long to leave the model alone.
func classify(status int, h http.Header, body []byte, ri *reqInfo) (failKind, time.Duration) {
	text := strings.ToLower(string(body))
	if len(text) > 4096 {
		text = text[:4096]
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	// OpenRouter wraps the upstream host's failure ("Provider returned error")
	// whatever its code: the model exists, its host is having trouble.
	if status != 429 && has("provider returned error") && !has("rate-limited", "rate limit", "quota") {
		return failTransient, 0
	}
	// "Your organization reached its limit of concurrent requests"
	// (requesty): a limit on parallel requests, not on the model's quota.
	if has("concurrent request", "concurrency limit", "too many concurrent") {
		return failBusy, 0
	}
	switch {
	case status == 401:
		return failAuth, 0
	case status == 402:
		// Payment required is about the model on this account (SambaNova's
		// DeepSeek-V3.2 on a free account); the provider's free models still work.
		return failPaid, 0
	case status == 403:
		// A refused key turns the provider off. A 403 about one model (OpenRouter:
		// "only available on agentic harnesses", NVIDIA: not enabled for the
		// account) only takes that model out.
		if has("api key", "api_key", "invalid key", "unauthorized", "authentication", "credential") {
			return failAuth, 0
		}
		return failNotFound, 0
	case status == 404:
		return failNotFound, 0
	case status == 429:
		return failQuota, quotaPause(h, text)
	case status == 413:
		return failTooLarge, 0
	case status == 429 && has("request too large"):
		return failTooLarge, 0
	case status == 400 || status == 422:
		switch {
		// OpenRouter sends a 400 with this text when the upstream is throttled.
		case has("rate-limited", "rate limit", "rate_limit", "quota"):
			return failQuota, quotaPause(h, text)
		case has("context length", "context_length", "maximum context", "too many tokens", "too long", "reduce the length"):
			return failTooLarge, 0
		case has("name") && has("extra_forbidden", "extra inputs", "not permitted", "unrecognized", "unknown field"):
			return failField, 0
		// Mistral: "top_p must be 1 when using greedy sampling" (temperature 0).
		case has("top_p must be 1"):
			return failField, 0
		// LLM7 passes Mistral's refusal of the name field on as this.
		case ri.hasNames && has("unprocessable"):
			return failField, 0
		case ri.hasTools && has("tool", "function"):
			return failUnsupported, 0
		case ri.hasImage && has("image", "vision", "multimodal"):
			return failUnsupported, 0
		case has("safety", "content policy", "content_filter", "moderation"):
			return failPolicy, 0
		case has("model") && has("not found", "does not exist", "not supported", "unknown"):
			return failNotFound, 0
		}
		return failClient, 0
	}
	return failTransient, 0
}

// quotaPause decides how long a model that hit a limit is left alone.
// Retry-After wins when present: it is the provider's answer for this very
// refusal. Without it the reset headers are used, the per-day ones only when
// the day's allowance is used up (Groq's x-ratelimit-reset-requests is the
// daily reset, so taking it for a per-minute refusal would pause for hours).
// Gemini puts the delay in the body as "retryDelay". With none of these it
// returns 0, and the model guesses from its own history (Model.guessPause).
func quotaPause(h http.Header, text string) time.Duration {
	var d time.Duration
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil {
			d = time.Duration(s * float64(time.Second))
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
	}
	if d <= 0 {
		if m := retryDelayRe.FindStringSubmatch(text); m != nil {
			d = parseReset(m[1])
		}
	}
	if d <= 0 {
		for _, k := range []string{"X-Ratelimit-Reset-Tokens", "X-Ratelimit-Reset"} {
			d = max(d, parseReset(h.Get(k)))
		}
		if h.Get("X-Ratelimit-Remaining-Requests") == "0" {
			d = max(d, parseReset(h.Get("X-Ratelimit-Reset-Requests")))
		}
		if h.Get("X-Ratelimit-Remaining-Requests-Day") == "0" {
			d = max(d, parseReset(h.Get("X-Ratelimit-Reset-Requests-Day")))
		}
	}
	if d <= 0 {
		return 0 // no reset time given: the model decides (Model.guessPause)
	}
	return min(d, maxQuotaPause)
}

// capRefusal says the refusal is an allowance used up (a day, a promotion),
// not a rate: it lasts long and its length is learned per model.
func capRefusal(msg string) bool {
	t := strings.ToLower(msg)
	for _, w := range []string{"promotion_model_limit_reached", "per day", "daily", "requests per day"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

var retryDelayRe = regexp.MustCompile(`"retrydelay"\s*:\s*"([0-9.]+s)"`)

// parseReset understands "7.66s", "2m59.56s", plain seconds, and unix
// timestamps in seconds or milliseconds.
func parseReset(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	now := time.Now()
	switch {
	case f > 1e12: // unix milliseconds
		return time.UnixMilli(int64(f)).Sub(now)
	case f > 1e9: // unix seconds
		return time.Unix(int64(f), 0).Sub(now)
	}
	return time.Duration(f * float64(time.Second))
}

// perMinuteCap reads "Limit 8000, Requested 14461" from a refusal about a
// per-minute token limit: no single request above the limit can pass, on
// any model of that tier, whatever its context window.
var perMinuteCapRe = regexp.MustCompile(`(?i)(?:per minute|tpm)[^0-9]*limit\s*(\d+)`)

func perMinuteCap(msg string) int {
	m := perMinuteCapRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
