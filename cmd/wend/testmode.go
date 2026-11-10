package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Test mode: a few short tasks with answers the router can check, to
// measure what a model is good at instead of guessing from its name. It is
// not a benchmark: eight calls, pass or fail each. Results set the model's
// measured strength (tier) and domains.

const (
	testVersion = 2 // 2: reasoning tasks may think aloud and end with ANSWER:
	testEvery   = 7 * 24 * time.Hour
)

// testPause is the wait between test calls, for per-minute limits.
var testPause = 2 * time.Second

type testCase struct {
	Name   string
	Domain string // code, reasoning, agents, vision, general
	Body   func() map[string]json.RawMessage
	Check  func(answer) (bool, string)
	Vision bool
}

// answer is what a test call returned.
type answer struct {
	Text  string
	Tools []*toolCall
}

// testResult is a model's last test run.
type testResult struct {
	Version int               `json:"version"`
	At      time.Time         `json:"at"`
	Passed  map[string]bool   `json:"passed"`
	Notes   map[string]string `json:"notes,omitempty"`
}

func (t *testResult) rate(domain string) (float64, int) {
	pass, n := 0, 0
	for _, c := range testCases {
		if c.Domain != domain {
			continue
		}
		if p, ok := t.Passed[c.Name]; ok {
			n++
			if p {
				pass++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(pass) / float64(n), n
}

// score is the share of text tests passed; vision is reported apart, so a
// text-only model is not marked down for being blind.
func (t *testResult) score() float64 {
	pass, n := 0, 0
	for _, c := range testCases {
		if c.Vision {
			continue
		}
		if p, ok := t.Passed[c.Name]; ok {
			n++
			if p {
				pass++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return float64(pass) / float64(n)
}

// tier turns a score into a strength tier, 1 strongest.
func (t *testResult) tier() int {
	switch s := t.score(); {
	case s >= 0.85:
		return 1
	case s >= 0.65:
		return 2
	case s >= 0.4:
		return 3
	}
	return 4
}

func msgs(content string) json.RawMessage {
	b, _ := json.Marshal([]map[string]any{{"role": "user", "content": content}})
	return b
}

func body(content string, maxTokens int) func() map[string]json.RawMessage {
	return func() map[string]json.RawMessage {
		return map[string]json.RawMessage{"messages": msgs(content), "max_tokens": json.RawMessage(fmt.Sprint(maxTokens))}
	}
}

var numberRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// lastNumber is the final number in a reply: models that explain before
// answering put the answer last.
func lastNumber(s string) string {
	all := numberRe.FindAllString(s, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

var answerLine = regexp.MustCompile(`(?i)answer\s*[:=]\s*\**\s*(-?\d+(?:\.\d+)?)`)

// wantNumber checks the number after "ANSWER:", or the last number when the
// model left that line out.
func wantNumber(n string) func(answer) (bool, string) {
	return func(a answer) (bool, string) {
		got := lastNumber(a.Text)
		if m := answerLine.FindAllStringSubmatch(a.Text, -1); len(m) > 0 {
			got = m[len(m)-1][1]
		}
		return got == n, "answered " + nonEmptyOr(got, "no number")
	}
}

// solve asks for a worked answer: a model may think it through, as on a
// real task, and ends with a line the router can read. Asking for the
// number alone measured guessing, not solving.
const solve = " Work it out step by step, then give the final answer on the last line as: ANSWER: <number>"

func nonEmptyOr(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

var testCases = []testCase{
	{
		Name: "instructions", Domain: "general",
		Body: body("Reply with exactly one word: avocado. No punctuation, nothing else.", 2000),
		Check: func(a answer) (bool, string) {
			got := strings.Trim(strings.TrimSpace(a.Text), ".!\"'`*")
			return strings.EqualFold(got, "avocado"), "answered " + truncate(got, 40)
		},
	},
	{
		Name: "json", Domain: "agents",
		Body: body(`Extract the facts from this sentence as a JSON object with keys "name" (string), "age" (number) and "city" (string). Output only the JSON.
Sentence: "Marta Kovac, 37, moved to Lisbon last spring."`, 2000),
		Check: func(a answer) (bool, string) {
			s := a.Text
			i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
			if i < 0 || j < i {
				return false, "no JSON object"
			}
			var v struct {
				Name string  `json:"name"`
				Age  float64 `json:"age"`
				City string  `json:"city"`
			}
			if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
				return false, "invalid JSON"
			}
			ok := strings.Contains(v.Name, "Marta") && v.Age == 37 && strings.EqualFold(v.City, "Lisbon")
			return ok, fmt.Sprintf("name %q, age %v, city %q", v.Name, v.Age, v.City)
		},
	},
	{
		Name: "tools", Domain: "agents",
		Body: func() map[string]json.RawMessage {
			return map[string]json.RawMessage{
				"messages":   msgs("What is the weather in Osaka right now, in celsius? Use the tool."),
				"max_tokens": json.RawMessage("2000"),
				"tools": json.RawMessage(`[{"type":"function","function":{"name":"get_weather","description":"Current weather for a city",` +
					`"parameters":{"type":"object","properties":{"city":{"type":"string"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["city"]}}}]`),
			}
		},
		Check: func(a answer) (bool, string) {
			for _, t := range a.Tools {
				if t.Function.Name != "get_weather" {
					continue
				}
				var args struct{ City, Unit string }
				if json.Unmarshal([]byte(t.Function.Arguments), &args) != nil {
					return false, "arguments are not JSON"
				}
				ok := strings.Contains(strings.ToLower(args.City), "osaka") && (args.Unit == "" || args.Unit == "celsius")
				return ok, fmt.Sprintf("city %q, unit %q", args.City, args.Unit)
			}
			return false, "no tool call"
		},
	},
	{
		Name: "reasoning-time", Domain: "reasoning",
		Body: body("A train leaves at 09:40 and arrives at 13:15. On the way it stops for 25 minutes in total. "+
			"How many minutes was it moving?"+solve, 3000),
		Check: wantNumber("190"),
	},
	{
		Name: "reasoning-boxes", Domain: "reasoning",
		Body: body("Three boxes hold 63 marbles together. Box A has twice as many as box B. Box C has 7 fewer than box A. "+
			"How many marbles are in box C?"+solve, 3000),
		Check: wantNumber("21"),
	},
	{
		Name: "code-trace", Domain: "code",
		Body: body("What does this Python program print?"+solve+"\n\n"+
			"def f(xs):\n    out = 0\n    for i, x in enumerate(xs):\n        if i % 2 == 0:\n            out += x * i\n"+
			"        else:\n            out -= x\n    return out\n\nprint(f([4, 1, 5, 2, 3]))", 3000),
		Check: wantNumber("19"),
	},
	{
		Name: "code-bug", Domain: "code",
		Body: body("This function should return the largest number in a non-empty list, but it has one bug. "+
			"Reply with only the corrected line of code.\n\n"+
			"def biggest(nums):\n    best = 0\n    for n in nums:\n        if n > best:\n            best = n\n    return best", 2000),
		Check: func(a answer) (bool, string) {
			s := strings.ReplaceAll(a.Text, " ", "")
			ok := strings.Contains(s, "best=nums[0]") || strings.Contains(s, "-inf") || strings.Contains(s, "-math.inf")
			return ok, "answered " + truncate(strings.TrimSpace(a.Text), 60)
		},
	},
	{
		Name: "vision", Domain: "vision", Vision: true,
		Body: func() map[string]json.RawMessage {
			content, _ := json.Marshal([]map[string]any{
				{"type": "text", "text": "What colour is the square in this image? Answer with one word."},
				{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + bluePNG()}},
			})
			m, _ := json.Marshal([]map[string]json.RawMessage{{"role": json.RawMessage(`"user"`), "content": content}})
			return map[string]json.RawMessage{"messages": m, "max_tokens": json.RawMessage("2000")}
		},
		Check: func(a answer) (bool, string) {
			return strings.Contains(strings.ToLower(a.Text), "blue"), "answered " + truncate(strings.TrimSpace(a.Text), 40)
		},
	},
}

var bluePNG = sync.OnceValue(func() string {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			c := color.RGBA{255, 255, 255, 255}
			if x >= 12 && x < 52 && y >= 12 && y < 52 {
				c = color.RGBA{20, 60, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return base64.StdEncoding.EncodeToString(b.Bytes())
})

// call sends one test request and returns the answer, recording the call
// like any other traffic (speed, health, reputation, cost).
func (r *Router) call(ctx context.Context, m *Model, name string, b map[string]json.RawMessage) (answer, *attemptErr) {
	ri := &reqInfo{model: m.Key, body: b, promptTokens: 200, outTokens: 1000, estTokens: 1200, probe: true, id: "test-" + newRequestID(), label: "test: " + name}
	ri.hasTools = len(b["tools"]) > 0
	ri.hasImage = bytes.Contains(b["messages"], []byte("image_url"))
	req, _ := http.NewRequestWithContext(ctx, "POST", "/test", nil)
	m.begin()
	defer m.end()
	u, ae := r.openStream(req, ri, m, probeTimeout)
	if ae != nil {
		r.noteFailure(m, ae, ri)
		return answer{}, ae
	}
	defer u.close()
	var a answer
	if u.whole != nil {
		var c struct {
			Choices []struct {
				Message struct {
					Content   string      `json:"content"`
					ToolCalls []*toolCall `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(u.whole, &c) == nil && len(c.Choices) > 0 {
			a = answer{Text: c.Choices[0].Message.Content, Tools: c.Choices[0].Message.ToolCalls}
		}
		ri.tokens = completionTokens(u.whole)
		ri.takeUsage(readUsage(u.whole))
	} else {
		var as assembler
		for _, l := range u.held {
			as.line(l)
		}
		for {
			line, err := u.br.ReadBytes('\n')
			if len(line) > 0 {
				if ae := as.line(line); ae != nil {
					r.noteFailure(m, ae, ri)
					return answer{}, ae
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				ae := netFailure(req, err)
				r.noteFailure(m, ae, ri)
				return answer{}, ae
			}
		}
		a.Text = as.content.String()
		for _, i := range slices.Sorted(mapKeys(as.tools)) {
			a.Tools = append(a.Tools, as.tools[i])
		}
		if as.reasoning.Len() > 0 {
			m.sawReasoning()
		}
		ri.tokens = as.tokens()
		ri.takeUsage(readUsage(as.usage))
	}
	ri.ttft, ri.total = u.ttft, time.Since(u.start)
	m.recordSuccess(ri.ttft, ri.total, ri.tokens)
	r.settle(m, ri)
	return a, nil
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func (r *Router) noteFailure(m *Model, ae *attemptErr, ri *reqInfo) {
	if ae.clientGone {
		return
	}
	if ae.kind == failAuth {
		m.Provider.refuseKey(truncate(ae.msg, 120))
	}
	ae.pause = m.recordFailure(ae.kind, ae.pause, ae.msg, ri)
}

// testModel runs every test on m. A run cut short by a quota, a refusal or
// a dead model is not saved: half a run would understate the model.
func (r *Router) testModel(ctx context.Context, m *Model) {
	res := &testResult{Version: testVersion, At: time.Now(), Passed: map[string]bool{}, Notes: map[string]string{}}
	// Pick up a run that a limit cut short within the last day.
	m.mu.Lock()
	if p := m.testPart; p != nil && p.Version == testVersion && time.Since(p.At) < 24*time.Hour {
		for k, v := range p.Passed {
			res.Passed[k], res.Notes[k] = v, p.Notes[k]
		}
	}
	m.mu.Unlock()
	keepPart := func() {
		m.mu.Lock()
		m.testPart = res
		m.mu.Unlock()
	}
	first := true
	for i, tc := range testCases {
		if i > 0 {
			r.tickRun(m) // the case before this one is behind us
		}
		if tc.Vision && m.capability("vision") == capNo {
			continue
		}
		if _, done := res.Passed[tc.Name]; done {
			continue
		}
		if !first {
			select {
			case <-ctx.Done():
				keepPart()
				return
			case <-time.After(testPause):
			}
		}
		first = false
		a, ae := r.call(ctx, m, tc.Name, tc.Body())
		// A short per-minute limit (OVH: 2 a minute) is waited out once.
		if ae != nil && ae.kind == failQuota && ae.pause > 0 && ae.pause <= 90*time.Second {
			select {
			case <-ctx.Done():
				return
			case <-time.After(ae.pause + time.Second):
			}
			a, ae = r.call(ctx, m, tc.Name, tc.Body())
		}
		if ae != nil {
			switch {
			case ae.clientGone:
				return
			case tc.Vision && ae.kind != failQuota && ae.kind != failAuth:
				// Text models answer an image with all sorts of errors; any of
				// them means no vision, not a broken model.
				res.Passed[tc.Name], res.Notes[tc.Name] = false, "images not accepted: "+ae.kind.String()
				m.mu.Lock()
				m.vision = capNo
				m.mu.Unlock()
				continue
			case ae.kind == failClient || ae.kind == failUnsupported:
				res.Passed[tc.Name], res.Notes[tc.Name] = false, ae.kind.String()+": "+truncate(oneLine(ae.msg), 80)
				continue
			}
			keepPart()
			log.Printf("test %s: stopped at %s (%s), %d of %d done; it goes on next time", m.Key, tc.Name, ae.kind, len(res.Passed), len(testCases))
			r.logTest(m, nil, fmt.Sprintf("stopped at %s: %s; %d done, goes on next time", tc.Name, ae.kind, len(res.Passed)))
			return
		}
		ok, note := tc.Check(a)
		res.Passed[tc.Name], res.Notes[tc.Name] = ok, note
		if tc.Vision {
			// The answer decides both ways: a text model that guessed "blue"
			// once must lose vision when it answers "unknown" the next time.
			m.mu.Lock()
			m.vision = capNo
			if ok {
				m.vision = capYes
			}
			m.mu.Unlock()
		}
	}
	m.setTest(res)
	r.logTest(m, res, "")
}

func (r *Router) logTest(m *Model, res *testResult, stopped string) {
	ri := &reqInfo{model: m.Key, probe: true}
	if res == nil {
		r.logRequest(ri, m.Key, "failed", []string{m.Key}, []string{"test " + stopped})
		return
	}
	var failed []string
	for _, c := range testCases {
		if p, ok := res.Passed[c.Name]; ok && !p {
			failed = append(failed, c.Name)
		}
	}
	summary := fmt.Sprintf("test %.0f%%, tier %d", res.score()*100, res.tier())
	if len(failed) > 0 {
		summary += ", missed " + strings.Join(failed, ", ")
	}
	r.logRequest(ri, m.Key, "tested", []string{m.Key}, []string{summary})
}

// setTest stores a run and applies it: measured tier and domains replace
// the seed's and the guesses from the name.
func (m *Model) setTest(res *testResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.test = res
	m.testPart = nil
	m.Tier = res.tier()
}

// testDue says whether a model should be tested now by the background loop.
func (r *Router) testDue(m *Model, now time.Time) bool {
	if m.paid() || m.Provider.Spec.TestManual || !m.usable(now) {
		return false
	}
	m.mu.Lock()
	t, part := m.test, m.testPart
	m.mu.Unlock()
	if part != nil {
		// Finish what a limit cut short; the failure that cut it was the
		// limit, so the model's last-attempt health does not count here.
		return true
	}
	if m.health() == healthFailing {
		return false
	}
	return t == nil || t.Version != testVersion || now.Sub(t.At) > testEvery
}

// testAll tests the given models, one model per provider at a time.
func (r *Router) testAll(ctx context.Context, models []*Model) {
	byProvider := map[*Provider][]*Model{}
	for _, m := range models {
		byProvider[m.Provider] = append(byProvider[m.Provider], m)
	}
	var wg sync.WaitGroup
	for p, list := range byProvider {
		wg.Go(func() {
			// A provider already being tested (the background loop) is
			// waited for, so a run from the dashboard covers every model.
			for {
				r.testMu.Lock()
				if !r.testing[p] {
					r.testing[p] = true
					r.testMu.Unlock()
					break
				}
				r.testMu.Unlock()
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			defer func() {
				r.testMu.Lock()
				delete(r.testing, p)
				r.testMu.Unlock()
			}()
			for _, m := range list {
				if ctx.Err() != nil {
					return
				}
				mctx, stop := r.runStarts(ctx, m)
				r.testModel(mctx, m)
				r.runEnds(m, mctx)
				stop()
			}
		})
	}
	wg.Wait()
}

// TestLoop tests untested models in the background, a little at a time:
// one model per provider every few minutes.
func (r *Router) TestLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		picked := map[*Provider]*Model{}
		r.mu.RLock()
		for _, m := range r.models {
			if _, taken := picked[m.Provider]; !taken && r.testDue(m, now) {
				picked[m.Provider] = m
			}
		}
		r.mu.RUnlock()
		var due []*Model
		for _, m := range picked {
			due = append(due, m)
		}
		if len(due) > 0 {
			r.testAll(ctx, due)
		}
	}
}

// handleTest tests one model (?model=provider/id) or every free model whose
// provider allows background tests.
func (r *Router) handleTest(w http.ResponseWriter, req *http.Request) {
	key := req.URL.Query().Get("model")
	now := time.Now()
	r.mu.RLock()
	var list []*Model
	for _, m := range r.models {
		switch {
		case m.paid():
		case key != "" && m.Key == key:
			list = append(list, m)
		case key == "" && !m.Provider.Spec.TestManual && m.usable(now):
			list = append(list, m)
		}
	}
	r.mu.RUnlock()
	if len(list) == 0 {
		writeError(w, 404, "no such free model", "invalid_request_error")
		return
	}
	if !r.startRun("test", list) {
		writeError(w, 409, "a test is already running; stop it or wait", "invalid_request_error")
		return
	}
	writeJSON(w, map[string]any{"testing": len(list), "calls_each": len(testCases)})
}

// testView is a model's test result for the dashboard.
type testView struct {
	At     string            `json:"at"`
	Score  float64           `json:"score"`
	Tier   int               `json:"tier"`
	Passed map[string]bool   `json:"passed"`
	Notes  map[string]string `json:"notes,omitempty"`
}

func (t *testResult) view() *testView {
	if t == nil {
		return nil
	}
	return &testView{At: t.At.Format(time.RFC3339), Score: t.score(), Tier: t.tier(), Passed: t.Passed, Notes: t.Notes}
}
