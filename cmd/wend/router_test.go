package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a provider that serves one model and answers chat requests with
// whatever handler the test gives it.
type fake struct {
	srv   *httptest.Server
	calls atomic.Int32
	chat  http.HandlerFunc
}

func newFake(t *testing.T, chat http.HandlerFunc) *fake {
	f := &fake{chat: chat}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
		case "/v1/chat/completions":
			f.calls.Add(1)
			f.chat(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newTestRouter builds a router over the fakes; the first fake gets tier 1,
// the next tier 2, and so on, so the order of attempts is known.
func newTestRouter(t *testing.T, fakes ...*fake) (*Router, *httptest.Server) {
	seed := &Seed{}
	for i, f := range fakes {
		seed.Providers = append(seed.Providers, ProviderSpec{
			ID: fmt.Sprintf("p%d", i+1), BaseURL: f.srv.URL + "/v1", KeyEnv: []string{"K"},
			Discover: "all", DefaultTier: i + 1,
		})
	}
	cfg := Config{TTFTTimeout: 300 * time.Millisecond, RequestTimeout: 2 * time.Second, MaxAttempts: 6, AllowTraining: true}
	r := NewRouter(seed, cfg, func(string) string { return "key" })
	r.Discover(context.Background())
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

func okJSON(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, text)
	}
}

func okStream(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		for _, word := range strings.Fields(text) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", word)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

func post(t *testing.T, srv *httptest.Server, body string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func statusOf(r *Router, key string) modelStatus {
	for _, s := range r.snapshot() {
		if s.Key == key {
			return s
		}
	}
	return modelStatus{}
}

const plain = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
const streaming = `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestQuotaFallsBackAndPauses(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limit"}}`)
	})
	b := newFake(t, okJSON("from b"))
	r, srv := newTestRouter(t, a, b)

	resp, body := post(t, srv, plain)
	if resp.StatusCode != 200 || !strings.Contains(body, "from b") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Router-Model"); got != "p2/m" {
		t.Fatalf("served by %s", got)
	}
	if s := statusOf(r, "p1/m"); s.State != "quota" {
		t.Fatalf("p1 state %s, want quota", s.State)
	}
	// While paused, p1 is not even tried.
	post(t, srv, plain)
	if n := a.calls.Load(); n != 1 {
		t.Fatalf("p1 called %d times, want 1", n)
	}
}

func TestEmptyStreamFallsBack(t *testing.T) {
	a := newFake(t, okStream(""))
	b := newFake(t, okStream("hello there"))
	_, srv := newTestRouter(t, a, b)

	resp, body := post(t, srv, streaming)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, `"there"`) {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("the empty stream leaked to the caller: %s", body)
	}
}

func TestSlowFirstTokenFallsBack(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	b := newFake(t, okStream("fast"))
	_, srv := newTestRouter(t, a, b)

	start := time.Now()
	resp, body := post(t, srv, streaming)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, "fast") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("fallback took %s, the first-token deadline is 300ms", took)
	}
}

func TestBreakerOpensAfterRepeatedFailures(t *testing.T) {
	a := newFake(t, status(502, "bad gateway"))
	r, srv := newTestRouter(t, a) // the only model, so every request tries it

	for range breakerThreshold {
		post(t, srv, plain)
	}
	if s := statusOf(r, "p1/m"); s.State != "open" {
		t.Fatalf("p1 state %s, want open", s.State)
	}
	resp, _ := post(t, srv, plain)
	if n := a.calls.Load(); n != breakerThreshold || resp.StatusCode != 503 {
		t.Fatalf("p1 called %d times with an open breaker, status %d", n, resp.StatusCode)
	}
}

func TestFailedModelGoesBehindHealthyOnes(t *testing.T) {
	a := newFake(t, status(502, "bad gateway"))
	b := newFake(t, okJSON("ok"))
	r, srv := newTestRouter(t, a, b) // p1 is the stronger tier
	post(t, srv, plain)
	post(t, srv, plain)
	if n := a.calls.Load(); n != 1 {
		t.Fatalf("p1 tried %d times; after failing once it should wait for a probe", n)
	}
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); got[0] != "p2/m" {
		t.Fatalf("order %v, want the healthy model first", got)
	}
}

func TestLearnsMissingToolSupport(t *testing.T) {
	a := newFake(t, status(400, `{"error":{"message":"this model does not support tools"}}`))
	b := newFake(t, okJSON("used tools"))
	r, srv := newTestRouter(t, a, b)

	withTools := `{"model":"auto","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	resp, _ := post(t, srv, withTools)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if s := statusOf(r, "p1/m"); s.Tools != "no" {
		t.Fatalf("p1 tools %s, want no", s.Tools)
	}
	post(t, srv, withTools)
	if n := a.calls.Load(); n != 1 {
		t.Fatalf("p1 tried again for tools: %d calls", n)
	}
	// A request without tools may still use it, after the models that work.
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); len(got) != 2 || got[1] != "p1/m" {
		t.Fatalf("plain request candidates %v", got)
	}
}

func TestClientErrorIsReturnedNotRetriedForever(t *testing.T) {
	bad := `{"error":{"message":"messages: field required"}}`
	a := newFake(t, status(400, bad))
	b := newFake(t, status(400, bad))
	c := newFake(t, okJSON("never"))
	_, srv := newTestRouter(t, a, b, c)

	resp, body := post(t, srv, plain)
	if resp.StatusCode != 400 || !strings.Contains(body, "field required") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if c.calls.Load() != 0 {
		t.Fatal("a third provider was tried for a request two providers rejected")
	}
}

func TestStickySession(t *testing.T) {
	var aDown atomic.Bool
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if aDown.Load() {
			status(503, "down")(w, r)
			return
		}
		okJSON("a")(w, r)
	})
	b := newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)

	aDown.Store(true)
	resp, _ := post(t, srv, plain, "X-Session-Id", "s1")
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatal("expected fallback to p2")
	}
	aDown.Store(false)
	resp, _ = post(t, srv, plain, "X-Session-Id", "s1")
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatalf("session moved back to %s; it should stay on the model that worked", resp.Header.Get("X-Router-Model"))
	}
	// p1 failed last time, so it waits behind p2 until a probe finds it well.
	resp, _ = post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatal("a model whose last attempt failed was put first")
	}
	r.probe(context.Background(), r.models[0])
	// The same conversation, recognised by its fingerprint, stays where it is.
	resp, _ = post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatal("a conversation without X-Session-Id moved model between its steps")
	}
	// A new conversation gets the strongest model again.
	resp, _ = post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"a new task"}]}`)
	if resp.Header.Get("X-Router-Model") != "p1/m" {
		t.Fatal("after a good probe the strongest model should lead a new conversation")
	}
	sess := r.book.active(time.Hour)
	if len(sess) != 3 {
		t.Fatalf("sessions: %d, want s1, the plain conversation and the new one", len(sess))
	}
}

func TestFingerprintFollowsTheConversation(t *testing.T) {
	step1 := json.RawMessage(`[{"role":"system","content":"You are Coder."},{"role":"user","content":"read fruit.csv"}]`)
	step2 := json.RawMessage(`[{"role":"system","content":"You are Coder."},{"role":"user","content":"read fruit.csv"},{"role":"assistant","content":"ok"},{"role":"tool","content":"apple,1"}]`)
	other := json.RawMessage(`[{"role":"system","content":"You are Coder."},{"role":"user","content":"something else"}]`)
	a, b, c := fingerprint("node", "coder", step1), fingerprint("node", "coder", step2), fingerprint("node", "coder", other)
	if a != b || a == c || a == fingerprint("python", "coder", step1) {
		t.Fatalf("fingerprints %s %s %s", a, b, c)
	}
}

func TestAllDownGives503(t *testing.T) {
	a := newFake(t, status(500, "x"))
	_, srv := newTestRouter(t, a)
	resp, body := post(t, srv, plain)
	var e struct {
		Error struct{ Type string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	if resp.StatusCode != 503 || e.Error.Type != "router_exhausted" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestQuotaPauseFromHeaders(t *testing.T) {
	// Retry-After is the answer for this refusal and wins.
	h := http.Header{}
	h.Set("Retry-After", "10")
	h.Set("X-Ratelimit-Reset-Requests", "2h")
	if d := quotaPause(h, ""); d != 10*time.Second {
		t.Fatalf("pause %s, want 10s", d)
	}
	// Groq: the requests reset is the daily one; only used when the day is spent.
	h = http.Header{}
	h.Set("X-Ratelimit-Reset-Requests", "2h")
	h.Set("X-Ratelimit-Remaining-Requests", "512")
	h.Set("X-Ratelimit-Reset-Tokens", "7.5s")
	if d := quotaPause(h, ""); d != 7500*time.Millisecond {
		t.Fatalf("pause %s, want 7.5s", d)
	}
	h.Set("X-Ratelimit-Remaining-Requests", "0")
	if d := quotaPause(h, ""); d != 2*time.Hour {
		t.Fatalf("pause %s, want 2h", d)
	}
	// Gemini: delay in the body.
	if d := quotaPause(http.Header{}, `{"error":{"details":[{"retrydelay": "33s"}]}}`); d != 33*time.Second {
		t.Fatalf("gemini pause %s", d)
	}
	// No reset time given: 0, the model guesses (TestGuessedPauses).
	if d := quotaPause(http.Header{}, "you exceeded requests per day"); d != 0 {
		t.Fatalf("no-hint pause %s, want 0", d)
	}
}

func TestGuessedPauses(t *testing.T) {
	m := &Model{Provider: &Provider{}}
	ri := &reqInfo{}
	// A rate refusal with no reset time: a minute, doubling while refusals keep coming.
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if got := m.recordFailure(failQuota, 0, "rate limit exceeded", ri); got != want {
			t.Fatalf("refusal %d: pause %s, want %s", i+1, got, want)
		}
	}
	m.recordSuccess(time.Second, 2*time.Second, 10)
	if got := m.recordFailure(failQuota, 0, "rate limit exceeded", ri); got != time.Minute {
		t.Fatalf("after an answer the backoff starts again: %s", got)
	}
	// The provider's own reset time always wins.
	if got := m.recordFailure(failQuota, 7*time.Second, "rate limit exceeded", ri); got != 7*time.Second {
		t.Fatalf("provider pause %s", got)
	}

	// An allowance used up (kilo without a key): 15 minutes the first time,
	// then as long as it stayed refused; the window is learned on the answer.
	k := &Model{Provider: &Provider{}}
	cap := `{"error":{"code":"PROMOTION_MODEL_LIMIT_REACHED","message":"Sign up for free"}}`
	if got := k.recordFailure(failQuota, 0, cap, ri); got != firstCapPause {
		t.Fatalf("first cap pause %s", got)
	}
	k.capSince = time.Now().Add(-40 * time.Minute) // as if refused for 40 minutes
	if got := k.recordFailure(failQuota, 0, cap, ri); got < 39*time.Minute || got > 41*time.Minute {
		t.Fatalf("still refused after 40 min: pause %s, want about 40 min", got)
	}
	k.recordSuccess(time.Second, 2*time.Second, 10)
	if k.capWindow < 39*time.Minute || !k.capSince.IsZero() {
		t.Fatalf("learned window %s", k.capWindow)
	}
	if got := k.recordFailure(failQuota, 0, cap, ri); got != k.capWindow {
		t.Fatalf("next cap pause %s, want the learned %s", got, k.capWindow)
	}
	// The window survives a restart.
	k2 := &Model{Provider: &Provider{}}
	k2.loadState(k.saveState())
	if k2.capWindow.Round(time.Second) != k.capWindow.Round(time.Second) {
		t.Fatalf("cap window after restart %s, want %s", k2.capWindow, k.capWindow)
	}
}

func TestSpeedNeedsAMeasurableWindow(t *testing.T) {
	// 300 tokens in 0.14 s is a provider sending a held answer at once.
	if v := tokensPerSecond(6900*time.Millisecond, 7040*time.Millisecond, 300); v != 0 {
		t.Fatalf("held answer measured as %.0f tok/s", v)
	}
	if v := tokensPerSecond(time.Second, 3*time.Second, 200); v != 100 {
		t.Fatalf("speed %.1f, want 100", v)
	}
	// A speed measured the old way is not loaded.
	m := &Model{Provider: &Provider{}}
	m.loadState(modelState{TPS: 2007})
	if m.tps != 0 {
		t.Fatalf("old speed loaded: %.0f", m.tps)
	}
	m.loadState(modelState{TPS: 80, TPSv: 2})
	if m.tps != 80 {
		t.Fatalf("speed %.0f, want 80", m.tps)
	}
}

func TestClassifyOpenRouterUpstreamThrottle(t *testing.T) {
	k, _ := classify(400, http.Header{}, []byte(`{"error":{"message":"Provider returned error","metadata":{"raw":"model is temporarily rate-limited upstream"}}}`), &reqInfo{})
	if k != failQuota {
		t.Fatalf("got %s, want quota", k)
	}
}

func TestBaseURLFromEnv(t *testing.T) {
	spec := ProviderSpec{ID: "cf", BaseURL: "https://x/accounts/{ACC}/ai/v1", KeyEnv: []string{"TOK"}, Discover: "seed-only",
		Models: []ModelSpec{{ID: "@cf/a"}}}
	env := map[string]string{"TOK": "t"}
	if p := newProvider(spec, func(k string) string { return env[k] }); p != nil {
		t.Fatal("provider enabled without the account id")
	}
	env["ACC"] = "123"
	p := newProvider(spec, func(k string) string { return env[k] })
	if p == nil || p.BaseURL != "https://x/accounts/123/ai/v1" {
		t.Fatalf("base url %v", p)
	}
	r := NewRouter(&Seed{Providers: []ProviderSpec{spec}}, Config{MaxAttempts: 1}, func(k string) string { return env[k] })
	r.Discover(context.Background())
	if len(r.models) != 1 || r.models[0].Key != "cf/@cf/a" {
		t.Fatalf("seed-only models: %v", r.models)
	}
}

func TestEmbeddedSeedLoads(t *testing.T) {
	s, err := loadSeed("")
	if err != nil || len(s.Providers) == 0 {
		t.Fatalf("seed: %v", err)
	}
}

func TestNonStreamCallerGetsFastFallbackAndAssembledAnswer(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	b := newFake(t, okStream("assembled from chunks"))
	_, srv := newTestRouter(t, a, b)

	start := time.Now()
	resp, body := post(t, srv, plain)
	if time.Since(start) > time.Second {
		t.Fatal("a non-streaming caller waited past the first-token deadline")
	}
	var c struct {
		Object  string
		Choices []struct{ Message struct{ Content string } }
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil || resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
	if c.Object != "chat.completion" || c.Choices[0].Message.Content != "assembledfromchunks" {
		t.Fatalf("assembled %q", body)
	}
}

func TestMidStreamBreakFallsBackForNonStreamCaller(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"upstream died\",\"code\":502}}\n\n")
	})
	b := newFake(t, okJSON("whole"))
	_, srv := newTestRouter(t, a, b)
	resp, body := post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, "whole") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
}

func TestAssemblerToolCalls(t *testing.T) {
	var a assembler
	for _, l := range []string{
		`data: {"id":"x","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Chicago\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":9}}`,
		`data: [DONE]`,
	} {
		if ae := a.line([]byte(l + "\n")); ae != nil {
			t.Fatal(ae.msg)
		}
	}
	var c struct {
		Choices []struct {
			Message struct {
				Content   *string
				ToolCalls []toolCall `json:"tool_calls"`
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage map[string]int
	}
	if err := json.Unmarshal(a.completion(), &c); err != nil {
		t.Fatal(err)
	}
	tc := c.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "get_weather" || tc[0].Function.Arguments != `{"city":"Chicago"}` {
		t.Fatalf("tool calls %+v", tc)
	}
	if c.Choices[0].Message.Content != nil || c.Choices[0].FinishReason != "tool_calls" || c.Usage["total_tokens"] != 9 {
		t.Fatalf("completion %+v", c)
	}
}

func TestForbiddenModelDoesNotTurnOffProvider(t *testing.T) {
	k, _ := classify(403, http.Header{}, []byte(`{"error":{"message":"x/y:free is only available on agentic harnesses"}}`), &reqInfo{})
	if k != failNotFound {
		t.Fatalf("model-level 403 classified %s", k)
	}
	k, _ = classify(403, http.Header{}, []byte(`{"error":{"message":"Invalid API key"}}`), &reqInfo{})
	if k != failAuth {
		t.Fatalf("key 403 classified %s", k)
	}
}

func TestOpenRouterWrappedUpstreamErrorIsTransient(t *testing.T) {
	k, _ := classify(404, http.Header{}, []byte(`{"error":{"message":"Provider returned error","code":404,"metadata":{"raw":"Not Found"}}}`), &reqInfo{})
	if k != failTransient {
		t.Fatalf("got %s, want transient", k)
	}
}

func TestEventsStreamRequestsAndStatus(t *testing.T) {
	a := newFake(t, okStream("one two three"))
	_, srv := newTestRouter(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		var buf strings.Builder
		b := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(b)
			buf.Write(b[:n])
			if strings.Contains(buf.String(), "event: request") || err != nil {
				got <- buf.String()
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	post(t, srv, streaming)
	s := <-got
	for _, want := range []string{"event: history", "event: status", "event: request", `"outcome":"ok"`, `"model":"p1/m"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("events missing %q:\n%s", want, s)
		}
	}
}

func TestPaymentRequiredTakesOutModelNotProvider(t *testing.T) {
	a := newFake(t, status(402, `{"error":{"code":"PAYMENT_METHOD_REQUIRED","message":"A payment method is required"}}`))
	b := newFake(t, okJSON("free one"))
	r, srv := newTestRouter(t, a, b)
	resp, _ := post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatal("no fallback after 402")
	}
	s := statusOf(r, "p1/m")
	if s.State != "gone" || !r.providers[0].usable(time.Now()) {
		t.Fatalf("402: model %s, provider usable %v", s.State, r.providers[0].usable(time.Now()))
	}
}

func TestDisabledProviderIsSkippedEvenWithKey(t *testing.T) {
	seed := &Seed{Providers: []ProviderSpec{{ID: "x", BaseURL: "http://127.0.0.1:1/v1", KeyEnv: []string{"K"}, Disabled: "needs a card"}}}
	r := NewRouter(seed, Config{MaxAttempts: 1}, func(string) string { return "key" })
	if len(r.providers) != 0 {
		t.Fatal("a disabled provider was enabled")
	}
	var b strings.Builder
	r.PrintTable(&b)
	if !strings.Contains(b.String(), "disabled: needs a card") {
		t.Fatalf("table:\n%s", b.String())
	}
}

// priced sets a model's price in USD per million tokens.
func priced(r *Router, key string, inM, outM float64) {
	for _, m := range r.models {
		if m.Key == key {
			m.priceIn, m.priceOut = inM/1e6, outM/1e6
		}
	}
}

func keys(ms []*Model) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Key)
	}
	return out
}

func TestCostCap(t *testing.T) {
	cheap := newFake(t, okJSON("cheap"))
	dear := newFake(t, okJSON("dear"))
	free := newFake(t, okJSON("free"))
	r, _ := newTestRouter(t, cheap, dear, free) // tiers 1, 2, 3
	priced(r, "p1/m", 0.10, 0.40)               // 1000 in + 1000 out = $0.0005
	priced(r, "p2/m", 3, 15)                    // same request = $0.018
	ri := &reqInfo{model: "auto", promptTokens: 1000, outTokens: 1000}

	r.cfg.MaxCost = 0
	if got := keys(r.candidates(ri)); len(got) != 1 || got[0] != "p3/m" {
		t.Fatalf("cap 0: %v, want only the free model", got)
	}
	r.cfg.MaxCost, r.cfg.DailyBudget = 0.0005, 1
	got := keys(r.candidates(ri))
	if len(got) != 2 || got[0] != "p3/m" || got[1] != "p1/m" {
		t.Fatalf("cap 0.0005: %v, want free first, then the cheap one, not the dear one", got)
	}
	r.spent.add("x", 1, false)
	if got := keys(r.candidates(ri)); len(got) != 1 {
		t.Fatalf("budget spent: %v, want only the free model", got)
	}
}

func TestSpendFromUsage(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":2000,"completion_tokens":500}}`)
	})
	r, srv := newTestRouter(t, a)
	priced(r, "p1/m", 1, 4) // 2000*1e-6 + 500*4e-6 = 0.004
	r.cfg.MaxCost, r.cfg.DailyBudget = 0.01, 1
	post(t, srv, plain)
	if s := r.spent.today(); s < 0.00399 || s > 0.00401 {
		t.Fatalf("spent %v, want 0.004", s)
	}
}

func setCaps(r *Router, key string, vision, reasoning capability, ttftMs float64) {
	for _, m := range r.models {
		if m.Key == key {
			m.vision, m.reasoning, m.ttftMs = vision, reasoning, ttftMs
		}
	}
}

func TestProfilesFilterAndOrder(t *testing.T) {
	a, b, c := newFake(t, okJSON("a")), newFake(t, okJSON("b")), newFake(t, okJSON("c"))
	r, _ := newTestRouter(t, a, b, c) // tiers 1, 2, 3
	setCaps(r, "p1/m", capNo, capYes, 3000)
	setCaps(r, "p2/m", capYes, capNo, 400)
	setCaps(r, "p3/m", capUnknown, capUnknown, 0)

	check := func(model string, want ...string) {
		t.Helper()
		got := keys(r.candidates(&reqInfo{model: model, outTokens: 1000}))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v, want %v", model, got, want)
		}
	}
	check("auto", "p1/m", "p2/m", "p3/m")
	check("vision", "p2/m")               // unknown vision does not count
	check("no-think", "p2/m", "p3/m")     // unknown reasoning is not excluded
	check("fast", "p2/m", "p1/m", "p3/m") // unmeasured last
	check("auto-fast", "p2/m", "p1/m", "p3/m")

	r.profiles["quick"] = &Profile{MaxTTFTMs: 1000}
	check("quick", "p2/m")
	r.profiles["none"] = &Profile{Require: []string{"tools"}, Models: []string{"nothing*"}}
	check("none")
	check("p3/m", "p3/m")              // that model at that provider only
	check("m", "p1/m", "p2/m", "p3/m") // that model wherever it is served
}

// A request is served by what it asked for and nothing else: a model nobody asked
// for, answering after minutes, is worse than an error within seconds.
func TestRequestsStayWithinWhatTheyAskedFor(t *testing.T) {
	a := newFake(t, status(500, `{"error":{"message":"down"}}`))
	b := newFake(t, okJSON("another model"))
	c := newFake(t, okJSON("the same model elsewhere"))
	r, srv := newTestRouter(t, a, b, c)
	for _, m := range r.models {
		if m.Key == "p2/m" { // p2 serves another model
			m.ID, m.Family = "other", "other"
		}
	}
	if got := strings.Join(keys(r.candidates(&reqInfo{model: "m"})), ","); got != "p1/m,p3/m" {
		t.Fatalf("a model name: got %s, want p1/m,p3/m (the same model elsewhere, never another)", got)
	}
	if got := strings.Join(keys(r.candidates(&reqInfo{model: "p1/m"})), ","); got != "p1/m" {
		t.Fatalf("provider/model: got %s, want p1/m alone", got)
	}
	ask := func(model string) (int, string) {
		resp, body := post(t, srv, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return resp.StatusCode, body
	}
	if code, body := ask("p1/m"); code == 200 {
		t.Fatalf("p1/m down: got %d %s, want an error: it asked for that one", code, body)
	}
	if code, body := ask("m"); code != 200 || !strings.Contains(body, "the same model elsewhere") {
		t.Fatalf("m down at p1: got %d %s, want the answer of p3/m", code, body)
	}
	c.chat = status(500, `{"error":{"message":"down"}}`)
	if code, body := ask("m"); code == 200 || strings.Contains(body, "another model") {
		t.Fatalf("its family down: got %d %s, want an error, not another model's answer", code, body)
	}

	if code, body := ask("no-such-model"); code != 404 || !strings.Contains(body, "model_not_found") {
		t.Fatalf("a name nobody serves: got %d %s, want 404 model_not_found", code, body)
	}

	r.profiles["only-m"] = &Profile{Models: []string{"nothing*"}}
	if got := keys(r.candidates(&reqInfo{model: "only-m"})); len(got) != 0 {
		t.Fatalf("a profile with no model up: got %v, want none", got)
	}
	if code, body := ask("only-m"); code == 200 || strings.Contains(body, "another model") {
		t.Fatalf("profile down: got %d %s, want an error", code, body)
	}
	r.profiles["only-m"].Fallback = []string{"p2/m"}
	if code, body := ask("only-m"); code != 200 || !strings.Contains(body, "another model") {
		t.Fatalf("a fallback the profile names: got %d %s, want its answer", code, body)
	}
}

func TestProfileAPI(t *testing.T) {
	t.Setenv("ROUTER_PROFILES", filepath.Join(t.TempDir(), "profiles.json"))
	a := newFake(t, okJSON("a"))
	r, srv := newTestRouter(t, a)

	put := func(name, body string) int {
		req, _ := http.NewRequest("PUT", srv.URL+"/api/profiles/"+name, strings.NewReader(body))
		req.Header.Set("X-Router-UI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("coder", `{"require":["tools"],"sort":"fastest","max_cost":0.0005}`); code != 200 {
		t.Fatalf("put: %d", code)
	}
	if code := put("bad", `{"require":["telepathy"]}`); code != 400 {
		t.Fatalf("bad capability accepted: %d", code)
	}
	if code := put("Bad Name", `{}`); code != 400 {
		t.Fatalf("bad name accepted: %d", code)
	}
	// Saved to disk and read back by a fresh load.
	r.profiles = nil
	if err := r.loadProfiles(); err != nil {
		t.Fatal(err)
	}
	if p := r.profile("coder"); p == nil || p.Sort != "fastest" || *p.MaxCost != 0.0005 {
		t.Fatalf("reloaded: %+v", p)
	}
	if r.profile("auto") == nil {
		t.Fatal("built-in profiles lost on reload")
	}
	resp, body := func() (*http.Response, string) {
		resp, err := http.Post(srv.URL+"/api/preview", "application/json", strings.NewReader(`{"require":["vision"]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}()
	if resp.StatusCode != 200 || !strings.Contains(body, `"reason":"no vision"`) {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}
}

func TestListPricesIgnoredOutsideFreeDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m","pricing":{"prompt":"0.00000015","completion":"0.0000006"}}]}`)
	}))
	t.Cleanup(srv.Close)
	for _, mode := range []string{"all", "free"} {
		seed := &Seed{Providers: []ProviderSpec{{ID: "p", BaseURL: srv.URL, KeyEnv: []string{"K"}, Discover: mode}}}
		r := NewRouter(seed, Config{MaxCost: 1, DailyBudget: 1}, func(string) string { return "k" })
		r.Discover(context.Background())
		if len(r.models) != 1 {
			t.Fatalf("%s: %d models", mode, len(r.models))
		}
		if paid := r.models[0].paid(); paid != (mode == "free") {
			t.Fatalf("%s: paid=%v", mode, paid)
		}
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	t.Setenv("ROUTER_STATE", filepath.Join(t.TempDir(), "state.json"))
	a := newFake(t, status(404, `{"error":"no such model"}`))
	b := newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)
	post(t, srv, plain) // p1 refused (gone for an hour), p2 served
	for _, m := range r.models {
		if m.Key == "p2/m" {
			m.tools = capNo
		}
	}
	r.spent.add("p2/m", 0.25, false)
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}

	r2 := NewRouter(r.seed, r.cfg, func(string) string { return "key" })
	if err := r2.LoadState(); err != nil {
		t.Fatal(err)
	}
	r2.Discover(context.Background())
	if s := statusOf(r2, "p1/m"); s.State != "gone" {
		t.Fatalf("p1 after restart: %s, want gone", s.State)
	}
	if s := statusOf(r2, "p2/m"); s.OK != 1 || s.Tools != "no" || s.Availability != 1 {
		t.Fatalf("p2 after restart: %+v", s)
	}
	if got := r2.spent.today(); got != 0.25 {
		t.Fatalf("spend after restart: %v", got)
	}
	if n := len(r2.live.recent()); n != 1 {
		t.Fatalf("request log after restart: %d entries", n)
	}
}

func TestReportedCostWinsAndSurprisesAreFlagged(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"cost\":0.0003}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	r, srv := newTestRouter(t, a)
	post(t, srv, streaming) // a "free" model that charged anyway
	post(t, srv, plain)     // and once more, assembled
	if got := r.spent.today(); got < 0.00059 || got > 0.00061 {
		t.Fatalf("spent %v, want 0.0006 from usage.cost", got)
	}
	if got := r.spent.surpriseToday(); got < 0.00059 {
		t.Fatalf("surprise %v, want the charge flagged", got)
	}
	log := r.live.recent()
	if log[len(log)-1].CostFrom != "provider" {
		t.Fatalf("cost source %q", log[len(log)-1].CostFrom)
	}
}

func TestProbeMeasuresAndMarksFailures(t *testing.T) {
	good := newFake(t, okStream("1 2 3 4 5 6 7 8 9 10"))
	bad := newFake(t, status(404, `{"error":"gone"}`))
	r, _ := newTestRouter(t, good, bad)
	r.cfg.ProbeEvery = time.Hour
	now := time.Now()
	for _, m := range r.models {
		if !r.needsProbe(m, now) {
			t.Fatalf("%s: an unmeasured model is due for a probe", m.Key)
		}
	}
	r.probeAll(context.Background(), r.models)
	if s := statusOf(r, "p1/m"); s.OK != 1 || s.State != "ok" {
		t.Fatalf("good model after probe: %+v", s)
	}
	if s := statusOf(r, "p2/m"); s.State != "gone" {
		t.Fatalf("bad model after probe: %s", s.State)
	}
	for _, m := range r.models {
		if r.needsProbe(m, time.Now()) {
			t.Fatalf("%s: probed again right away", m.Key)
		}
	}
	log := r.live.recent()
	if len(log) != 2 || !log[0].Probe {
		t.Fatalf("probe log: %+v", log)
	}
	// Paid models are never probed.
	priced(r, "p1/m", 1, 1)
	if r.needsProbe(r.models[0], time.Now().Add(2*time.Hour)) && r.models[0].paid() {
		t.Fatal("a paid model is due for a probe")
	}
}

func TestSealRoundTrip(t *testing.T) {
	t.Setenv("ROUTER_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	plain := []byte(`{"keys":{"groq":[{"key":"gsk_secret"}]}}`)
	sealed, err := seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "gsk_secret") {
		t.Fatal("the sealed file holds the key in the clear")
	}
	back, err := unseal(sealed)
	if err != nil || string(back) != string(plain) {
		t.Fatalf("unseal: %v %q", err, back)
	}
}

// keyedFake answers /models only for the accepted key.
func keyedFake(t *testing.T, accepted string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accepted {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"invalid api key"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
		default:
			okJSON("hi")(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func uiCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("X-Router-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAddKeyInDashboard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROUTER_MASTER_KEY_FILE", filepath.Join(dir, "master.key"))
	up := keyedFake(t, "good-key-123456")
	seed := &Seed{Providers: []ProviderSpec{{ID: "acme", BaseURL: up.URL + "/v1", KeyEnv: []string{"ACME_KEY"}, Discover: "all"}}}
	r := NewRouter(seed, Config{MaxAttempts: 3, TTFTTimeout: time.Second, RequestTimeout: time.Second}, func(string) string { return "" })
	ks, err := openKeyStore(filepath.Join(dir, "keys.dat"))
	if err != nil {
		t.Fatal(err)
	}
	r.store = ks
	r.reloadProviders()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	if len(r.providerList()) != 0 {
		t.Fatal("a provider without a key is on")
	}

	// The guard: no header, no change.
	resp, _ := http.Post(srv.URL+"/api/providers/acme/keys", "application/json", strings.NewReader(`{"key":"x"}`))
	if resp.StatusCode != 403 {
		t.Fatalf("change without the UI header: %d", resp.StatusCode)
	}
	// A refused key is not stored.
	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/acme/keys", `{"key":"bad-key-0000000"}`); code != 400 || !strings.Contains(body, "refused") {
		t.Fatalf("bad key: %d %s", code, body)
	}
	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/acme/keys", `{"key":"good-key-123456","label":"main"}`); code != 200 {
		t.Fatalf("good key: %d %s", code, body)
	}
	if p := r.providerByID("acme"); p == nil || len(p.keyList()) != 1 {
		t.Fatal("provider not on after adding a key")
	}
	// The list shows the key masked, never in full.
	resp, _ = http.Get(srv.URL + "/api/providers")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "good-key-123456") || !strings.Contains(string(b), "good-k") {
		t.Fatalf("provider list leaks or hides the key: %s", b)
	}
	// Stored encrypted and read back by a new store.
	raw, _ := os.ReadFile(filepath.Join(dir, "keys.dat"))
	if strings.Contains(string(raw), "good-key-123456") {
		t.Fatal("key file is not encrypted")
	}
	ks2, err := openKeyStore(filepath.Join(dir, "keys.dat"))
	if err != nil || len(ks2.view().Keys["acme"]) != 1 {
		t.Fatalf("reopen: %v", err)
	}
	// Turning the provider off takes it out.
	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/acme/enabled", `{"enabled":false}`); code != 200 {
		t.Fatalf("disable: %d %s", code, body)
	}
	if r.providerByID("acme") != nil {
		t.Fatal("disabled provider still on")
	}
}

func TestRefusedKeyMovesToNext(t *testing.T) {
	up := keyedFake(t, "second-key-99999")
	p := newProvider(ProviderSpec{ID: "x", BaseURL: up.URL + "/v1", Discover: "all"}, func(string) string { return "" },
		providerKey{ID: "a", Value: "first-key-000000"}, providerKey{ID: "b", Value: "second-key-99999"})
	seed := &Seed{}
	r := NewRouter(seed, Config{MaxAttempts: 2}, func(string) string { return "" })
	r.providers = []*Provider{p}
	r.Discover(context.Background())
	if len(r.models) != 1 {
		t.Fatalf("models after key rotation: %d (state %s)", len(r.models), p.listErr)
	}
	if ks := p.keyList(); ks[0].BadWhy == "" || ks[1].BadWhy != "" || !p.usable(time.Now()) {
		t.Fatalf("keys after rotation: %+v", ks)
	}
}

func TestRediscoveryKeepsRefusals(t *testing.T) {
	a := newFake(t, status(402, `{"error":"not included in your free usage"}`))
	r, srv := newTestRouter(t, a)
	post(t, srv, plain)
	r.Discover(context.Background())
	if s := statusOf(r, "p1/m"); s.State != "gone" {
		t.Fatalf("after rediscovery: %s, want gone until the paid-only mark expires", s.State)
	}
}

func TestReputation(t *testing.T) {
	var r reputation
	if r.label() != "new" {
		t.Fatal("no evidence should read as new")
	}
	for range 20 {
		r.add(true)
	}
	if r.label() != "stable" {
		t.Fatalf("20 good: %s", r.label())
	}
	for range 10 {
		r.add(false)
	}
	if r.label() != "flaky" {
		t.Fatalf("20 good, 10 bad: %s", r.label())
	}
	// Old outcomes fade: a week later, a few good answers outweigh them.
	r.restore(repData{Good: r.Good, Total: r.Total, At: time.Now().Add(-7 * 24 * time.Hour)})
	for range 6 {
		r.add(true)
	}
	if r.flaky() {
		s, n := r.share()
		t.Fatalf("a model that got better stays flaky: %.2f of %.1f", s, n)
	}
}

func TestFlakyGoesBehindReliable(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b) // p1 stronger
	post(t, srv, plain)
	for range 10 {
		r.models[0].rep.add(false) // p1 usually fails
	}
	for range 10 {
		r.models[1].rep.add(true)
	}
	r.models[1].recordSuccess(time.Millisecond, time.Millisecond, 1) // both answered last time
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); got[0] != "p2/m" {
		t.Fatalf("order %v: a flaky model leads a reliable one", got)
	}
}

func TestDomains(t *testing.T) {
	if d := guessDomains("poolside/laguna-s-2.1:free", ""); !slices.Contains(d, "code") {
		t.Fatalf("laguna: %v", d)
	}
	if d := guessDomains("x/model", "A model built for agentic coding tasks."); !slices.Contains(d, "code") {
		t.Fatalf("description: %v", d)
	}
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	r, _ := newTestRouter(t, a, b)
	r.models[0].tags = []string{"code"}
	r.models[0].tools = capYes
	r.profiles["coder"] = &Profile{Domains: []string{"code", "agents"}}
	if got := keys(r.candidates(&reqInfo{model: "coder", outTokens: 1000})); len(got) != 1 || got[0] != "p1/m" {
		t.Fatalf("coder profile: %v", got)
	}
	if err := (&Profile{Domains: []string{"poetry"}}).validate(); err == nil {
		t.Fatal("unknown domain accepted")
	}
}

func TestPinnedModelsLead(t *testing.T) {
	a, b, c := newFake(t, okJSON("a")), newFake(t, okJSON("b")), newFake(t, okJSON("c"))
	r, srv := newTestRouter(t, a, b, c)
	r.profiles["auto"] = &Profile{Pinned: []string{"p3/m", "p2/m"}}
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); strings.Join(got, ",") != "p3/m,p2/m,p1/m" {
		t.Fatalf("pinned order: %v", got)
	}
	resp, _ := post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p3/m" {
		t.Fatal("the first pinned model did not serve")
	}
	r.profiles["only"] = &Profile{Pinned: []string{"p2/m"}, OnlyPinned: true}
	if got := keys(r.candidates(&reqInfo{model: "only", outTokens: 1000})); strings.Join(got, ",") != "p2/m" {
		t.Fatalf("only pinned: %v", got)
	}
}

func TestFallbackGoesLastEvenOutsideFilters(t *testing.T) {
	a, b, c := newFake(t, okJSON("a")), newFake(t, okJSON("b")), newFake(t, okJSON("c"))
	r, _ := newTestRouter(t, a, b, c)
	r.models[1].vision = capYes // only p2 can see
	r.profiles["vis"] = &Profile{Require: []string{"vision"}, Fallback: []string{"p1/m"}}
	if got := keys(r.candidates(&reqInfo{model: "vis", outTokens: 1000})); strings.Join(got, ",") != "p2/m,p1/m" {
		t.Fatalf("vision with a fallback: %v", got)
	}
	r.profiles["auto"] = &Profile{Fallback: []string{"p1/m"}}
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); got[len(got)-1] != "p1/m" {
		t.Fatalf("fallback not last: %v", got)
	}
}

// smartFake answers the test prompts correctly, as a good model would.
func smartFake(t *testing.T) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		w.Header().Set("Content-Type", "application/json")
		reply := func(text string) { fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, text) }
		switch {
		case strings.Contains(s, "avocado"):
			reply("avocado")
		case strings.Contains(s, "Marta"):
			reply(`{"name":"Marta Kovac","age":37,"city":"Lisbon"}`)
		case strings.Contains(s, "get_weather"):
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Osaka\",\"unit\":\"celsius\"}"}}]}}]}`)
		case strings.Contains(s, "train"):
			reply("215 minutes in total, 25 of them stopped.\nANSWER: 190")
		case strings.Contains(s, "marbles"):
			reply("21")
		case strings.Contains(s, "enumerate"):
			reply("19")
		case strings.Contains(s, "biggest"):
			reply("    best = nums[0]")
		case strings.Contains(s, "image_url"):
			reply("Blue.")
		default:
			reply("?")
		}
	})
}

func TestTestModeMeasuresTierAndDomains(t *testing.T) {
	testPause = 0
	good, dumb := smartFake(t), newFake(t, okJSON("I am not sure."))
	r, _ := newTestRouter(t, dumb, good) // the dumb one has the stronger seed tier
	r.models[0].tags = []string{"code"}  // its name claims coding
	for _, m := range r.models {
		r.testModel(context.Background(), m)
	}
	gs, ds := statusOf(r, "p2/m"), statusOf(r, "p1/m")
	if gs.Test == nil || gs.Test.Score != 1 || gs.Tier != 1 || !slices.Contains(gs.Domains, "code") || !slices.Contains(gs.Domains, "vision") {
		t.Fatalf("good model: %+v", gs)
	}
	if ds.Test == nil || ds.Test.Score != 0 || ds.Tier != 4 || slices.Contains(ds.Domains, "code") {
		t.Fatalf("dumb model: tier %d score %+v domains %v", ds.Tier, ds.Test, ds.Domains)
	}
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); got[0] != "p2/m" {
		t.Fatalf("measured strength did not reorder: %v", got)
	}
	r.cfg.ProbeEvery = time.Hour
	if r.testDue(r.models[0], time.Now()) {
		t.Fatal("tested again right away")
	}
}

func TestTrafficLogRecordsEverythingButKeys(t *testing.T) {
	dir := t.TempDir()
	a := newFake(t, status(503, `{"error":"busy"}`))
	b := newFake(t, okStream("hello there"))
	r, srv := newTestRouter(t, a, b)
	r.traffic = &trafficLog{dir: dir, days: 7}
	r.traffic.on.Store(true)
	post(t, srv, streaming)
	r.traffic.mu.Lock()
	r.traffic.f.Close()
	r.traffic.f = nil
	r.traffic.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(dir, "traffic-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("log files: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	kinds := map[string]int{}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev struct{ ID, Kind string }
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad line %q", line)
		}
		kinds[ev.Kind]++
		ids[ev.ID] = true
	}
	if kinds["client_request"] != 1 || kinds["upstream_request"] != 2 || kinds["upstream_response"] != 2 || kinds["client_response"] != 1 || len(ids) != 1 {
		t.Fatalf("events %v, ids %d", kinds, len(ids))
	}
	s := string(raw)
	if !strings.Contains(s, `"busy"`) || !strings.Contains(s, "there") {
		t.Fatal("the raw responses are missing")
	}
	if strings.Contains(s, "Bearer") || strings.Contains(s, `"key"`) {
		t.Fatal("a key reached the traffic log")
	}
}

func TestProactiveLimits(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "300")
		w.Header().Set("X-Ratelimit-Reset-Tokens", "30s")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.Header().Set("X-Ratelimit-Reset-Requests", "45s")
		okJSON("a")(w, r)
	})
	b := newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)
	post(t, srv, plain)
	if s := statusOf(r, "p1/m"); s.State != "quota" || s.Limits["tokens"].Left != 300 {
		t.Fatalf("after a response with no requests left: %+v", s)
	}
	// Tokens alone: a big request goes elsewhere, a small one may stay.
	for _, m := range r.models {
		if m.Key == "p1/m" {
			m.pausedUntil = time.Time{}
		}
	}
	big := &reqInfo{model: "auto", estTokens: 5000, outTokens: 1000}
	small := &reqInfo{model: "auto", estTokens: 100, outTokens: 100}
	if got := keys(r.candidates(big)); slices.Contains(got, "p1/m") {
		t.Fatalf("a request over the tokens left still went to p1: %v", got)
	}
	if got := keys(r.candidates(small)); !slices.Contains(got, "p1/m") {
		t.Fatalf("a small request was kept off p1: %v", got)
	}
}

func TestTestRunResumesAfterLimit(t *testing.T) {
	testPause = 0
	var limited atomic.Bool
	limited.Store(true)
	smart := smartFake(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if limited.Load() && strings.Contains(string(b), "marbles") {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":"rate limit"}`)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		smart.chat(w, r)
	})
	r, _ := newTestRouter(t, f)
	m := r.models[0]
	r.testModel(context.Background(), m)
	if m.test != nil || m.testPart == nil || len(m.testPart.Passed) == 0 {
		t.Fatalf("after a cut-short run: test %v, part %v", m.test, m.testPart)
	}
	done := len(m.testPart.Passed)
	if !r.testDue(m, time.Now().Add(2*time.Hour)) {
		t.Fatal("a cut-short run is not due to be finished")
	}
	limited.Store(false)
	m.pausedUntil = time.Time{}
	r.testModel(context.Background(), m)
	if m.test == nil || m.testPart != nil || m.test.score() != 1 {
		t.Fatalf("after the second run: %+v (had %d done before)", m.test, done)
	}
}

func postMessages(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAnthropicMessagesPlain(t *testing.T) {
	var got []byte
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	})
	_, srv := newTestRouter(t, a)
	code, body := postMessages(t, srv, `{"model":"claude-sonnet-4-5","max_tokens":100,"system":"Be brief.","messages":[{"role":"user","content":"Hi"}]}`)
	var m struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct{ Type, Text string }
		Usage      struct{ InputTokens, OutputTokens int } `json:"usage"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &m) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if len(m.Content) != 1 || m.Content[0].Text != "Hello!" || m.StopReason != "end_turn" {
		t.Fatalf("response %s", body)
	}
	if !strings.Contains(string(got), `"role":"system"`) || !strings.Contains(string(got), `"model":"m"`) {
		t.Fatalf("upstream got %s", got)
	}
}

func TestAnthropicToolRoundTrip(t *testing.T) {
	var got []byte
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	_, srv := newTestRouter(t, a)
	req := `{"model":"claude-x","max_tokens":200,"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
	 "messages":[{"role":"user","content":"weather in Paris then Oslo"},
	   {"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}]},
	   {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"12C"},{"type":"text","text":"now Oslo"}]}]}`
	code, body := postMessages(t, srv, req)
	if code != 200 || !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"stop_reason":"tool_use"`) || !strings.Contains(body, `"city":"Oslo"`) {
		t.Fatalf("%d %s", code, body)
	}
	s := string(got)
	for _, want := range []string{`"role":"tool"`, `"tool_call_id":"call_1"`, `"tool_calls"`, `"name":"get_weather"`, `"parameters"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("upstream body lacks %s: %s", want, s)
		}
	}
	if strings.Index(s, `"role":"tool"`) > strings.Index(s, "now Oslo") {
		t.Fatal("the tool result must come before the user's next text")
	}
}

func TestAnthropicStreaming(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Let me check.\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Rome\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	_, srv := newTestRouter(t, a)
	code, body := postMessages(t, srv, `{"model":"claude-x","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	order := []string{"event: message_start", `"type":"text_delta"`, `"type":"tool_use"`, `"partial_json":"{\"city\":"`, "event: content_block_stop", `"stop_reason":"tool_use"`, "event: message_stop"}
	at := 0
	for _, want := range order {
		i := strings.Index(body[at:], want)
		if i < 0 {
			t.Fatalf("missing or out of order %q in:\n%s", want, body)
		}
		at += i
	}
}

func TestAnthropicError(t *testing.T) {
	a := newFake(t, status(500, "boom"))
	_, srv := newTestRouter(t, a)
	code, body := postMessages(t, srv, `{"model":"claude-x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 503 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "overloaded_error") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestGroqPerMinuteCapIsNotContext(t *testing.T) {
	groq := `{"error":{"message":"Request too large for model openai/gpt-oss-120b in organization x service tier on_demand on tokens per minute (TPM): Limit 8000, Requested 14461, please reduce your message size and try again.","type":"tokens","code":"rate_limit_exceeded"}}`
	a := newFake(t, status(413, groq))
	b := newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)
	big := `{"model":"auto","messages":[{"role":"user","content":"` + strings.Repeat("x", 60000) + `"}]}`
	post(t, srv, big)
	s := statusOf(r, "p1/m")
	if s.MaxRequest != 8000 || s.Context != 0 {
		t.Fatalf("after the TPM refusal: max request %d, context %d", s.MaxRequest, s.Context)
	}
	if got := keys(r.candidates(&reqInfo{model: "auto", estTokens: 2000, outTokens: 500})); !slices.Contains(got, "p1/m") {
		t.Fatalf("a small request was kept off the capped model: %v", got)
	}
}

func TestRejectedNameFieldIsDroppedAndPrivateFieldsNeverSent(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), `"name"`) {
			w.WriteHeader(422)
			fmt.Fprint(w, `{"error":{"origin":"provider","message":"{\"detail\":[{\"type\":\"extra_forbidden\",\"loc\":[\"body\",\"messages\",1,\"user\",\"name\"],\"msg\":\"Extra inputs are not permitted\"}]}"}}`)
			return
		}
		okJSON("fine")(w, r)
	})
	b := newFake(t, status(503, "down")) // so the second request has to use p1
	r, srv := newTestRouter(t, a, b)
	req := `{"model":"auto","messages":[{"role":"user","name":"agent@host","content":"hi"},{"role":"tool","tool_call_id":"1","_denied":false,"content":"x"}]}`
	post(t, srv, req)
	if s := statusOf(r, "p1/m"); !s.DropNames {
		t.Fatalf("name refusal not learned: %+v", s)
	}
	resp, _ := post(t, srv, strings.Replace(req, "hi", "hi again", 1))
	if resp.Header.Get("X-Router-Model") != "p1/m" {
		t.Fatal("the model that refused name was not used again without it")
	}
	mu.Lock()
	defer mu.Unlock()
	last := bodies[len(bodies)-1]
	if strings.Contains(last, `"name"`) {
		t.Fatalf("name still sent: %s", last)
	}
	for _, body := range bodies {
		if strings.Contains(body, "_denied") {
			t.Fatalf("a private field reached the provider: %s", body)
		}
	}
}

func TestThinkingThatEatsTheLimitFallsBack(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Let me analyse the request...\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	b := newFake(t, okJSON(`{"intent":"file_read"}`))
	r, srv := newTestRouter(t, a, b)
	resp, body := post(t, srv, `{"model":"auto","max_tokens":400,"messages":[{"role":"user","content":"classify"}]}`)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, "file_read") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
	if s := statusOf(r, "p1/m"); s.Reasoning != "yes" {
		t.Fatalf("thinking not learned: %s", s.Reasoning)
	}
}

func TestStickinessIsPerProfile(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)
	r.profiles["fast"] = &Profile{Sort: "fastest"}
	r.remember("s|fast", "p2/m") // the classifier's model
	resp, _ := post(t, srv, plain, "X-Session-Id", "s")
	if resp.Header.Get("X-Router-Model") != "p1/m" {
		t.Fatal("the agent's profile was pulled onto the classifier's model")
	}
}

func TestMistralTopPQuirk(t *testing.T) {
	var last string
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = string(b)
		if !strings.Contains(last, `"top_p":1`) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"origin":"provider","message":"top_p must be 1 when using greedy sampling."}}`)
			return
		}
		okJSON("ok")(w, r)
	})
	b := newFake(t, status(503, "down"))
	r, srv := newTestRouter(t, a, b)
	greedy := `{"model":"auto","temperature":0,"messages":[{"role":"user","content":"x"}]}`
	post(t, srv, greedy)
	if s := statusOf(r, "p1/m"); !s.TopP1 || s.State != "ok" {
		t.Fatalf("quirk not learned: %+v", s)
	}
	resp, _ := post(t, srv, strings.Replace(greedy, `"x"`, `"y"`, 1))
	if resp.Header.Get("X-Router-Model") != "p1/m" {
		t.Fatalf("not served with top_p 1: %s", last)
	}
}

func TestPinnedOnlyProfileWaitsOutAShortPause(t *testing.T) {
	a := newFake(t, okJSON("after the pause"))
	r, srv := newTestRouter(t, a)
	r.cfg.MaxWait = 3 * time.Second
	r.profiles["only"] = &Profile{Pinned: []string{"p1/m"}, OnlyPinned: true}
	r.models[0].pausedUntil = time.Now().Add(700 * time.Millisecond)
	start := time.Now()
	resp, body := post(t, srv, `{"model":"only","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "after the pause") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Fatalf("answered in %s, before the pause ended", took)
	}
	// A pause longer than the wait still fails at once.
	r.models[0].pausedUntil = time.Now().Add(time.Hour)
	if resp, _ := post(t, srv, `{"model":"only","messages":[{"role":"user","content":"y"}]}`); resp.StatusCode != 503 {
		t.Fatalf("long pause: %d", resp.StatusCode)
	}
}

func TestSessionsCountTokensPerClient(t *testing.T) {
	var down atomic.Bool
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			status(503, "down")(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1200,"completion_tokens":30}}`)
	})
	r, srv := newTestRouter(t, a)
	for i := 0; i < 2; i++ {
		if resp, _ := post(t, srv, plain, "X-Session-Id", "s1", "X-Router-Client", "coder"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	// A request no model could serve counts as failed and adds no tokens.
	down.Store(true)
	post(t, srv, plain, "X-Session-Id", "s1", "X-Router-Client", "coder")

	var s *sessionInfo
	for _, x := range r.book.active(time.Hour) {
		if x.ID == "s1" {
			s = &x
		}
	}
	if s == nil {
		t.Fatal("session s1 not recorded")
	}
	if s.Client != "coder" || s.Requests != 3 || s.Failed != 1 || s.InFlight != 0 {
		t.Fatalf("session: %+v", *s)
	}
	if s.TokensIn != 2400 || s.TokensOut != 60 {
		t.Fatalf("tokens in/out %d/%d, want 2400/60", s.TokensIn, s.TokensOut)
	}
	if tm := r.tokensLastMinute(); tm["in"] != 2400 || tm["out"] != 60 {
		t.Fatalf("tokens in the last minute: %v", tm)
	}
}

func TestConcurrencyLimitFillsTheProviderNotTheModel(t *testing.T) {
	var inflight atomic.Int32
	release := make(chan struct{})
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if inflight.Add(1) > 1 {
			inflight.Add(-1)
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"origin":"router","message":"Your organization reached its limit of concurrent requests."}}`)
			return
		}
		<-release
		inflight.Add(-1)
		okJSON("a")(w, r)
	})
	b := newFake(t, okJSON("b"))
	r, srv := newTestRouter(t, a, b)

	// The first request holds p1; the second is refused there as busy and goes to p2.
	first := make(chan string)
	go func() {
		resp, _ := post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"one"}]}`)
		first <- resp.Header.Get("X-Router-Model")
	}()
	for inflight.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	resp, _ := post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"two"}]}`)
	if got := resp.Header.Get("X-Router-Model"); got != "p2/m" {
		t.Fatalf("second request served by %s, want p2/m", got)
	}
	p1 := r.models[0]
	if !p1.pausedUntil.IsZero() || p1.Provider.busyLimit != 1 {
		t.Fatalf("busy refusal: paused until %v, provider limit %d", p1.pausedUntil, p1.Provider.busyLimit)
	}
	// While p1's one slot is taken, a third request skips it without asking.
	calls := a.calls.Load()
	resp, _ = post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"three"}]}`)
	if resp.Header.Get("X-Router-Model") != "p2/m" || a.calls.Load() != calls {
		t.Fatalf("third request: %s, p1 asked %d more times", resp.Header.Get("X-Router-Model"), a.calls.Load()-calls)
	}
	close(release)
	if got := <-first; got != "p1/m" {
		t.Fatalf("first request served by %s", got)
	}
	// With the slot free p1 takes requests again.
	resp, _ = post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"four"}]}`)
	if resp.Header.Get("X-Router-Model") != "p1/m" {
		t.Fatalf("after the slot freed: %s", resp.Header.Get("X-Router-Model"))
	}
}

func TestPipeShowsTheRawTrafficLiveToThisMachineOnly(t *testing.T) {
	a := newFake(t, okStream("hello from the pipe"))
	r, srv := newTestRouter(t, a)
	// sent before anyone watches: a tab opened later still shows it
	post(t, srv, `{"model":"auto","messages":[{"role":"user","content":"before you looked"}]}`)
	// A page from elsewhere that resolves to 127.0.0.1 still carries its own Host.
	req, _ := http.NewRequest("GET", srv.URL+"/pipe", nil)
	req.Host = "evil.example"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign Host: %v %v", resp.StatusCode, err)
	}
	t.Setenv("ROUTER_REMOTE_PIPE", "1")
	ctx, cancel := context.WithCancel(context.Background())
	if resp, err := http.DefaultClient.Do(req.Clone(ctx)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("foreign Host with ROUTER_REMOTE_PIPE=1: %v %v", resp.StatusCode, err)
	}
	cancel()
	t.Setenv("ROUTER_REMOTE_PIPE", "")

	resp, err := http.Get(srv.URL + "/pipe")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("watch: %v %v", resp, err)
	}
	defer resp.Body.Close()
	events := make(chan pipeEvent, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e pipeEvent
				if json.Unmarshal([]byte(data), &e) == nil {
					events <- e
				}
			}
		}
	}()
	post(t, srv, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"show me"}]}`)

	var kinds []string
	var upstreamText, clientText, all strings.Builder
	deadline := time.After(3 * time.Second)
	var lastN int64
	for ends := 0; ends < 2; { // the kept request, then the live one
		select {
		case e := <-events:
			if e.N <= lastN {
				t.Fatalf("frame %d after %d", e.N, lastN)
			}
			lastN = e.N
			if len(kinds) == 0 || kinds[len(kinds)-1] != e.Kind {
				kinds = append(kinds, e.Kind)
			}
			all.WriteString(e.Text)
			switch e.Kind {
			case "upstream_chunk":
				upstreamText.WriteString(e.Text)
			case "client_chunk":
				clientText.WriteString(e.Text)
			case "client_end":
				ends++
			}
		case <-deadline:
			t.Fatalf("pipe stopped after %v", kinds)
		}
	}
	if !strings.Contains(all.String(), "before you looked") || !strings.Contains(all.String(), "show me") {
		t.Fatal("the pipe lost the request sent before the tab was opened, or the live one")
	}
	// The stream is relayed as it comes, so model and client chunks interleave.
	for _, k := range []string{"client_request", "upstream_request", "upstream_status", "upstream_chunk", "client_chunk", "client_end"} {
		if !slices.Contains(kinds, k) {
			t.Fatalf("no %s in %v", k, kinds)
		}
	}
	if kinds[0] != "client_request" || kinds[len(kinds)-1] != "client_end" {
		t.Fatalf("order: %v", kinds)
	}
	if !strings.Contains(upstreamText.String(), `"content":"hello"`) || !strings.Contains(clientText.String(), `"content":"pipe"`) {
		t.Fatalf("raw stream not shown:\nupstream %q\nclient %q", upstreamText.String(), clientText.String())
	}
	if strings.Contains(all.String(), "Bearer") {
		t.Fatal("a key went through the pipe")
	}
	// The router's own checks go out too, so they are shown, marked as such.
	// The client request's last events (its upstream_end) may still be on the way.
	r.probe(context.Background(), r.models[0])
	timeout := time.After(time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind != "upstream_request" {
				continue
			}
			if e.Label != "check" || !strings.Contains(e.Text, probePrompt) {
				t.Fatalf("a check in the pipe: %q %q", e.Label, e.Text)
			}
			return
		case <-timeout:
			t.Fatal("a check did not show in the pipe")
		}
	}
}

// stallAfterToken sends one token and then nothing, holding the stream open.
func stallAfterToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
	w.(http.Flusher).Flush()
	select {
	case <-time.After(3 * time.Second):
	case <-r.Context().Done():
	}
}

func modelByKey(r *Router, key string) *Model {
	for _, m := range r.models {
		if m.Key == key {
			return m
		}
	}
	return nil
}

func TestFirstTokenDeadlineFollowsTheModel(t *testing.T) {
	m := &Model{}
	lo, hi := 8*time.Second, 45*time.Second
	if d := m.firstTokenDeadline(lo, hi, 0); d != hi {
		t.Fatalf("unmeasured model: %s, want %s", d, hi)
	}
	m.ttftMs, m.ok = 800, 2
	if d := m.firstTokenDeadline(lo, hi, 0); d != hi {
		t.Fatalf("two answers are too few to trust: %s", d)
	}
	m.ok = 5
	if d := m.firstTokenDeadline(lo, hi, 0); d != lo {
		t.Fatalf("fast model: %s, want the floor %s", d, lo)
	}
	m.ttftMs = 5000
	if d := m.firstTokenDeadline(lo, hi, 0); d != 20*time.Second {
		t.Fatalf("5 s model: %s, want 20s", d)
	}
	if d := m.firstTokenDeadline(lo, hi, 50000); d != 30*time.Second {
		t.Fatalf("5 s model, 50K prompt: %s, want 30s", d)
	}
	if d := m.firstTokenDeadline(0, hi, 0); d != hi {
		t.Fatalf("adaptation off: %s", d)
	}
	m.ttftMs = 1000
	m.noteSlow(9 * time.Second)
	if m.ttftMs <= 1000 {
		t.Fatal("a given-up wait did not slow the model down")
	}
	before := m.ttftMs
	m.noteSlow(time.Millisecond)
	if m.ttftMs != before {
		t.Fatal("noteSlow made the model faster")
	}
}

func TestKnownFastModelIsGivenUpSooner(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	b := newFake(t, okStream("fast"))
	r, srv := newTestRouter(t, a, b)
	r.cfg.TTFTTimeout, r.cfg.TTFTMin = 2*time.Second, 100*time.Millisecond
	ma := modelByKey(r, "p1/m")
	ma.ttftMs, ma.ok = 10, 5 // fast so far

	start := time.Now()
	resp, body := post(t, srv, streaming)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, "fast") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), body)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("fallback took %s; the fast model's deadline is 100ms, not the 2s ceiling", took)
	}
	if ma.sortTTFT() <= 10 {
		t.Fatalf("the slow attempt left the model ranked fast: %v ms", ma.sortTTFT())
	}
}

func TestStallMidAnswerFallsBackForNonStreaming(t *testing.T) {
	a := newFake(t, stallAfterToken)
	b := newFake(t, okStream("whole answer"))
	r, srv := newTestRouter(t, a, b)
	r.cfg.StallTimeout = 200 * time.Millisecond

	start := time.Now()
	resp, body := post(t, srv, plain)
	if resp.StatusCode != 200 || resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(body, "answer") {
		t.Fatalf("got %d %s / %s", resp.StatusCode, resp.Header.Get("X-Router-Model"), body)
	}
	if strings.Contains(body, "half") {
		t.Fatalf("the stalled answer leaked: %s", body)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("took %s", took)
	}
}

func TestStallMidStreamEndsWithAnError(t *testing.T) {
	a := newFake(t, stallAfterToken)
	r, srv := newTestRouter(t, a)
	r.cfg.StallTimeout = 200 * time.Millisecond

	start := time.Now()
	_, body := post(t, srv, streaming)
	if !strings.Contains(body, "half") || !strings.Contains(body, "upstream_stalled") {
		t.Fatalf("want the partial answer and a stall error, got %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("a stalled stream must not end as complete: %s", body)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("took %s", took)
	}
	if ma := modelByKey(r, "p1/m"); ma.errs == 0 {
		t.Fatal("the stall was not counted against the model")
	}
}

func TestStallMidStreamAnthropicError(t *testing.T) {
	a := newFake(t, stallAfterToken)
	r, srv := newTestRouter(t, a)
	r.cfg.StallTimeout = 200 * time.Millisecond

	_, body := postMessages(t, srv, `{"model":"auto","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, "event: error") || strings.Contains(body, "message_stop") {
		t.Fatalf("want an error event and no message_stop, got %s", body)
	}
}

func TestKeepAliveDoesNotHoldOffTheStallWatch(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
		w.(http.Flusher).Flush()
		for range 30 {
			select {
			case <-time.After(50 * time.Millisecond):
				fmt.Fprint(w, ": still processing\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	b := newFake(t, okStream("whole answer"))
	r, srv := newTestRouter(t, a, b)
	r.cfg.StallTimeout = 200 * time.Millisecond

	resp, _ := post(t, srv, plain)
	if resp.Header.Get("X-Router-Model") != "p2/m" {
		t.Fatalf("keep-alive comments kept a silent model on: %s", resp.Header.Get("X-Router-Model"))
	}
}

func TestLastModelGetsTheFullFirstTokenDeadline(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(400 * time.Millisecond): // slow for this model, within the ceiling
		case <-r.Context().Done():
			return
		}
		okStream("late but fine")(w, r)
	})
	r, srv := newTestRouter(t, a) // the only model
	r.cfg.TTFTTimeout, r.cfg.TTFTMin = 2*time.Second, 100*time.Millisecond
	ma := modelByKey(r, "p1/m")
	ma.ttftMs, ma.ok = 10, 5

	resp, body := post(t, srv, streaming)
	if resp.StatusCode != 200 || !strings.Contains(body, "fine") {
		t.Fatalf("the only model was cut off early: %d %s", resp.StatusCode, body)
	}
}

// slowThen answers after d, or not at all when the request is cancelled.
func slowThen(d time.Duration, text string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		okStream(text)(w, r)
	}
}

func TestHedgeRacesASlowModel(t *testing.T) {
	var ha, hb atomic.Int32
	a := newFake(t, slowThen(1500*time.Millisecond, "slow one", &ha))
	b := newFake(t, slowThen(0, "quick one", &hb))
	r, srv := newTestRouter(t, a, b)
	r.cfg.TTFTTimeout = 3 * time.Second
	r.profiles["race"] = &Profile{HedgeAfterMs: 200}

	start := time.Now()
	for _, body := range []string{
		`{"model":"race","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"race","messages":[{"role":"user","content":"another conversation"}]}`,
	} {
		resp, out := post(t, srv, body)
		if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(out, "quick") || strings.Contains(out, "slow") {
			t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), out)
		}
	}
	if took := time.Since(start); took > 1400*time.Millisecond {
		t.Fatalf("two hedged requests took %s; each should take about 200ms", took)
	}
	// After losing a race the slow model may drop behind; either way b
	// answered both and a was asked at least once.
	if ha.Load() < 1 || hb.Load() != 2 {
		t.Fatalf("hits a=%d b=%d", ha.Load(), hb.Load())
	}
}

func TestHedgeLeavesAFastModelAlone(t *testing.T) {
	var ha, hb atomic.Int32
	a := newFake(t, slowThen(0, "first", &ha))
	b := newFake(t, slowThen(0, "second", &hb))
	r, srv := newTestRouter(t, a, b)
	r.profiles["race"] = &Profile{HedgeAfterMs: 300}

	resp, out := post(t, srv, `{"model":"race","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.Header.Get("X-Router-Model") != "p1/m" || !strings.Contains(out, "first") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), out)
	}
	if hb.Load() != 0 {
		t.Fatal("a fast first model still got a second request")
	}
}

func TestHedgeFallsBackWhenTheFirstFailsEarly(t *testing.T) {
	var hb atomic.Int32
	a := newFake(t, status(502, "bad gateway"))
	b := newFake(t, slowThen(0, "backup", &hb))
	r, srv := newTestRouter(t, a, b)
	r.profiles["race"] = &Profile{HedgeAfterMs: 1000}

	start := time.Now()
	resp, out := post(t, srv, `{"model":"race","messages":[{"role":"user","content":"hi"}]}`)
	if resp.Header.Get("X-Router-Model") != "p2/m" || !strings.Contains(out, "backup") || hb.Load() != 1 {
		t.Fatalf("got %s / %s, backup hits %d", resp.Header.Get("X-Router-Model"), out, hb.Load())
	}
	if time.Since(start) > 800*time.Millisecond {
		t.Fatal("an early failure waited for the hedge delay")
	}
}

func TestHedgeBothSlowFirstAnswerWins(t *testing.T) {
	var ha, hb atomic.Int32
	a := newFake(t, slowThen(600*time.Millisecond, "alpha", &ha))
	b := newFake(t, slowThen(900*time.Millisecond, "beta", &hb))
	r, srv := newTestRouter(t, a, b)
	r.cfg.TTFTTimeout = 3 * time.Second
	r.profiles["race"] = &Profile{HedgeAfterMs: 100}

	resp, out := post(t, srv, `{"model":"race","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.Header.Get("X-Router-Model") != "p1/m" || !strings.Contains(out, "alpha") || strings.Contains(out, "beta") {
		t.Fatalf("got %s / %s", resp.Header.Get("X-Router-Model"), out)
	}
}

func TestPromptEstimateCountsImagesAsTokensNotBytes(t *testing.T) {
	img := `"data:image/png;base64,` + strings.Repeat("A", 400000) + `"`
	msgs := []byte(`[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":` + img + `}}]}]`)
	got := promptEstimate(msgs, nil)
	if got < imageTokens || got > imageTokens+100 {
		t.Fatalf("a request with one screenshot estimated at %d tokens, want about %d", got, imageTokens)
	}
}

func TestUsageLedgerCountsAnswersAndFailures(t *testing.T) {
	a := newFake(t, status(502, "bad gateway"))
	b := newFake(t, okStream("one two three"))
	r, srv := newTestRouter(t, a, b)
	post(t, srv, streaming, "X-Router-Client", "tester")

	days := r.usage.report(1)
	if len(days) != 1 {
		t.Fatalf("days: %+v", days)
	}
	d := days[0]
	if d.Total.Requests != 2 || d.Total.OK != 1 || d.Total.Failed != 1 {
		t.Fatalf("total %+v, want one failure and one answer", d.Total)
	}
	if d.ByModel["p2/m"] == nil || d.ByModel["p2/m"].TokensIn == 0 || d.ByModel["p2/m"].TokensOut == 0 {
		t.Fatalf("by model %+v", d.ByModel)
	}
	if d.ByModel["p1/m"].Failed != 1 {
		t.Fatalf("the failed model was not booked: %+v", d.ByModel["p1/m"])
	}
	if d.Total.EstimatedIn != d.Total.TokensIn {
		t.Fatalf("the fake reports no usage, so all input is estimated: %+v", d.Total)
	}
	if len(d.ByClient) != 1 {
		t.Fatalf("by client %+v", d.ByClient)
	}
}

func TestUsageLedgerUsesReportedTokensAndSurvivesARestart(t *testing.T) {
	b := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1234,\"completion_tokens\":56}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	r, srv := newTestRouter(t, b)
	post(t, srv, streaming)
	d := r.usage.report(1)[0].Total
	if d.TokensIn != 1234 || d.TokensOut != 56 || d.EstimatedIn != 0 || d.EstimatedOut != 0 {
		t.Fatalf("reported usage not used: %+v", d)
	}

	t.Setenv("ROUTER_STATE", filepath.Join(t.TempDir(), "state.json"))
	if err := r.SaveState(); err != nil {
		t.Fatal(err)
	}
	r2, _ := newTestRouter(t, b)
	if err := r2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if got := r2.usage.report(1)[0].Total; got.TokensIn != 1234 {
		t.Fatalf("after a restart: %+v", got)
	}

	rec := httptest.NewRecorder()
	r2.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage?days=7", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tokens_in":1234`) {
		t.Fatalf("/api/usage: %d %s", rec.Code, rec.Body.String())
	}
}

// The help is served to a person and to an agent alike: an index of every page, each
// page as Markdown with its commands pointing at the address that was used.
func TestHelpPages(t *testing.T) {
	_, srv := newTestRouter(t, newFake(t, okJSON("a")))
	get := func(path string) (int, string, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	code, typ, index := get("/help")
	if code != 200 || !strings.HasPrefix(typ, "text/markdown") {
		t.Fatalf("/help: %d %s", code, typ)
	}
	for _, p := range helpPages {
		if !strings.Contains(index, srv.URL+"/help/"+p+".md") {
			t.Errorf("the index does not list %s", p)
		}
		code, _, text := get("/help/" + p + ".md")
		if code != 200 || !strings.HasPrefix(text, "# ") {
			t.Errorf("/help/%s.md: %d %.40q", p, code, text)
		}
		if strings.Contains(text, "127.0.0.1:8888") {
			t.Errorf("/help/%s.md still names 127.0.0.1:8888 instead of this router", p)
		}
	}
	if code, _, _ := get("/help/nothing.md"); code != 404 {
		t.Errorf("/help/nothing.md: %d, want 404", code)
	}
	if code, typ, _ := get("/ui/help.js"); code != 200 || !strings.HasPrefix(typ, "text/javascript") {
		t.Errorf("/ui/help.js: %d %s", code, typ)
	}
}
