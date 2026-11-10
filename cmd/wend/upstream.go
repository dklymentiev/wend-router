package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Every call to a model is made as a stream, whatever the caller asked for.
// The first real token is the moment of truth: before it, a model that
// fails, stalls or sends nothing is swapped for the next one unnoticed.
// A caller that asked for a stream gets it relayed from that moment; a
// caller that did not gets the stream assembled into one response, and
// since nothing has reached it until the end, even a break midway can
// still fall back to the next model.

// upstreamBody is the caller's request with the model's own id, always
// streaming.
func (r *Router) upstreamBody(ri *reqInfo, m *Model) ([]byte, error) {
	body := make(map[string]json.RawMessage, len(ri.body)+1)
	for k, v := range ri.body {
		body[k] = v
	}
	body["model"], _ = json.Marshal(m.ID)
	body["stream"] = json.RawMessage("true")
	m.mu.Lock()
	dropNames, topP1 := m.dropNames, m.topP1
	m.mu.Unlock()
	if topP1 && string(body["temperature"]) == "0" {
		body["top_p"] = json.RawMessage("1")
	}
	if msgs, ok := cleanMessages(body["messages"], dropNames); ok {
		body["messages"] = msgs
	}
	return json.Marshal(body)
}

func (r *Router) doUpstream(ctx context.Context, ri *reqInfo, m *Model, payload []byte) (*http.Response, error) {
	url := m.Provider.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	m.Provider.authorize(req)
	start := time.Now()
	resp, err := r.client.Do(req)
	r.logUpstream(ri, m, url, payload, resp, err, start)
	r.pipeUpstream(ri, m, payload, resp, err, start)
	return resp, err
}

// upstream is an open stream that has delivered its first real token.
type upstream struct {
	resp   *http.Response
	br     *bufio.Reader
	held   [][]byte // lines read up to and including the first token
	start  time.Time
	ttft   time.Duration
	whole  []byte // set instead of a stream when the provider answered in one piece
	cancel context.CancelFunc

	stall   time.Duration // the longest silence allowed after the first token
	watch   *time.Timer
	stalled atomic.Bool
}

func (u *upstream) close() {
	if u.watch != nil {
		u.watch.Stop()
	}
	u.cancel()
	u.resp.Body.Close()
}

// guard starts the watch on the rest of the stream: a model that sends no
// data for d is cut off, so a stalled answer fails instead of hanging.
func (u *upstream) guard(d time.Duration) {
	if d <= 0 || u.br == nil {
		return
	}
	u.stall = d
	u.watch = time.AfterFunc(d, func() { u.stalled.Store(true); u.cancel() })
}

// readLine reads the next stream line; every data line restarts the watch.
// Keep-alive comments do not: a model that only says it is still busy is
// not answering.
func (u *upstream) readLine() ([]byte, error) {
	line, err := u.br.ReadBytes('\n')
	if err == nil && u.watch != nil && sseData(line) != nil {
		u.watch.Reset(u.stall)
	}
	return line, err
}

func (u *upstream) stallMsg() string {
	return fmt.Sprintf("the model sent nothing for %s mid-answer", u.stall)
}

// openStream calls m and reads until the first real token. deadline, when
// set, bounds the whole call, not just the first token.
func (r *Router) openStream(req *http.Request, ri *reqInfo, m *Model, deadline time.Duration) (*upstream, *attemptErr) {
	payload, err := r.upstreamBody(ri, m)
	if err != nil {
		return nil, &attemptErr{kind: failClient, msg: err.Error()}
	}
	ctx, cancel := context.WithCancel(req.Context())
	if deadline > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, deadline)
		prev := cancel
		cancel = func() { c2(); prev() }
	}
	// A model known to be fast is not waited on for as long as a slow one:
	// when it stops being fast, the next model takes over sooner.
	// The last model left gets the full deadline: giving up on a slow answer
	// helps only when another model can take over.
	limit := r.cfg.TTFTTimeout
	if !ri.lastChance {
		limit = m.firstTokenDeadline(r.cfg.TTFTMin, r.cfg.TTFTTimeout, ri.promptTokens)
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(limit, func() { timedOut.Store(true); cancel() })
	noToken := &attemptErr{kind: failTransient, msg: fmt.Sprintf("no first token within %s", limit.Round(100*time.Millisecond))}
	fail := func(ae *attemptErr) (*upstream, *attemptErr) {
		timer.Stop()
		cancel()
		if ae == noToken {
			m.noteSlow(limit)
		}
		return nil, ae
	}

	start := time.Now()
	resp, err := r.doUpstream(ctx, ri, m, payload)
	if err != nil {
		if timedOut.Load() {
			return fail(noToken)
		}
		return fail(netFailure(req, err))
	}
	failResp := func(ae *attemptErr) (*upstream, *attemptErr) {
		resp.Body.Close()
		return fail(ae)
	}
	m.noteLimits(resp.Header)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		kind, pause := classify(resp.StatusCode, resp.Header, body, ri)
		return failResp(&attemptErr{kind: kind, pause: pause, status: resp.StatusCode, body: body, msg: string(body)})
	}
	u := &upstream{resp: resp, start: start, cancel: cancel}

	// Some providers answer in one piece even when asked for a stream.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		switch {
		case err != nil && timedOut.Load():
			return failResp(noToken)
		case err != nil:
			return failResp(netFailure(req, err))
		}
		if status, ok := embeddedError(body); ok {
			kind, pause := classify(status, resp.Header, body, ri)
			return failResp(&attemptErr{kind: kind, pause: pause, status: status, body: body, msg: string(body)})
		}
		if emptyCompletion(body) {
			return failResp(&attemptErr{kind: failEmpty, msg: "the model answered with nothing"})
		}
		if !timer.Stop() && timedOut.Load() {
			return failResp(noToken)
		}
		u.whole, u.ttft = body, time.Since(start)
		return u, nil
	}

	u.br = bufio.NewReaderSize(resp.Body, 64<<10)
	size := 0
	for {
		line, err := u.br.ReadBytes('\n')
		if len(line) > 0 {
			u.held = append(u.held, line)
			size += len(line)
		}
		if err != nil {
			switch {
			case timedOut.Load():
				return failResp(noToken)
			case req.Context().Err() != nil:
				return failResp(&attemptErr{clientGone: true})
			case err == io.EOF:
				return failResp(&attemptErr{kind: failEmpty, msg: "the stream ended before any token"})
			}
			return failResp(&attemptErr{kind: failTransient, msg: "stream: " + err.Error()})
		}
		ev := sseData(line)
		if ev == nil {
			if size > 1<<20 {
				return failResp(&attemptErr{kind: failTransient, msg: "no token in the first megabyte of the stream"})
			}
			continue
		}
		if string(ev) == "[DONE]" {
			return failResp(&attemptErr{kind: failEmpty, msg: "the stream ended before any token"})
		}
		kind, status := inspectChunk(ev)
		if kind == chunkError {
			k, pause := classify(status, resp.Header, ev, ri)
			return failResp(&attemptErr{kind: k, pause: pause, status: status, body: ev, msg: string(ev)})
		}
		if kind == chunkToken {
			break
		}
	}
	if !timer.Stop() && timedOut.Load() {
		return failResp(noToken)
	}
	u.ttft = time.Since(start)
	return u, nil
}

// serveOnce reads an opened answer to its end and sends it in one piece.
func (r *Router) serveOnce(w http.ResponseWriter, req *http.Request, ri *reqInfo, m *Model, u *upstream, tried []string) *attemptErr {
	defer u.close()
	body := u.whole
	if body == nil {
		u.guard(r.cfg.StallTimeout)
		var a assembler
		for _, l := range u.held {
			a.line(l)
		}
		for {
			line, err := u.readLine()
			if len(line) > 0 {
				if ae := a.line(line); ae != nil {
					return ae
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				if req.Context().Err() != nil {
					return &attemptErr{clientGone: true}
				}
				if u.stalled.Load() {
					// Nothing has reached the caller yet: the next model answers.
					m.noteSlow(u.stall)
					return &attemptErr{kind: failTransient, msg: u.stallMsg()}
				}
				return &attemptErr{kind: failTransient, msg: "stream broke: " + err.Error()}
			}
		}
		if a.empty() {
			return &attemptErr{kind: failEmpty, msg: "the model answered with nothing"}
		}
		// A thinking model that spent the whole token limit thinking gave no
		// answer; nothing reached the caller yet, so another model may give one.
		if a.finish == "length" && a.content.Len() == 0 && len(a.tools) == 0 && a.reasoning.Len() > 0 {
			m.sawReasoning()
			return &attemptErr{kind: failEmpty, msg: "spent the whole token limit thinking, no answer"}
		}
		body = a.completion()
		ri.tokens = a.tokens()
		if a.reasoning.Len() > 0 {
			m.sawReasoning()
		}
		ri.takeUsage(readUsage(a.usage))
	} else {
		ri.tokens = completionTokens(body)
		ri.takeUsage(readUsage(body))
	}
	ri.ttft, ri.total = u.ttft, time.Since(u.start)
	m.recordSuccess(ri.ttft, ri.total, ri.tokens)
	body = r.applyResponse(ri.session, body)
	setRouterHeaders(w, m, tried)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
	return nil
}

// serveStream relays an opened stream to the caller as it comes.
func (r *Router) serveStream(w http.ResponseWriter, req *http.Request, ri *reqInfo, m *Model, u *upstream, tried []string) *attemptErr {
	defer u.close()
	if u.whole != nil {
		return &attemptErr{kind: failTransient, msg: "asked for a stream, got a single response"}
	}

	setRouterHeaders(w, m, tried)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	for _, l := range u.held {
		if out := r.applyChunk(ri.session, l); out != nil {
			_, _ = w.Write(out)
		}
	}
	flush()
	u.guard(r.cfg.StallTimeout)
	failedMidway := false
	var chars, thought, usageTokens int
	for _, l := range u.held {
		if ev := sseData(l); ev != nil {
			n, t := outputChars(ev)
			chars, thought = chars+n, thought+t
		}
	}
	for {
		line, err := u.readLine()
		if len(line) > 0 {
			if ev := sseData(line); ev != nil {
				n, t := outputChars(ev)
				chars, thought = chars+n, thought+t
				if u := readUsage(ev); u.completion > 0 || u.cost != nil {
					usageTokens = max(usageTokens, u.completion)
					ri.takeUsage(u)
				}
				if k, _ := inspectChunk(ev); k == chunkError {
					failedMidway = true
					m.recordFailure(failTransient, 0, "error in stream: "+string(ev), ri)
				}
			}
			if out := r.applyChunk(ri.session, line); out != nil {
				_, _ = w.Write(out)
			}
			if len(bytes.TrimSpace(line)) == 0 {
				flush()
			}
		}
		if err != nil {
			for _, t := range r.pipeline {
				if out := t.Flush(ri.session); out != nil {
					_, _ = w.Write(out)
				}
			}
			flush()
			switch {
			case err == io.EOF:
				if thought > 0 {
					m.sawReasoning()
				}
				if !failedMidway {
					ri.ttft, ri.total, ri.tokens = u.ttft, time.Since(u.start), estimateTokens(chars, usageTokens)
					m.recordSuccess(ri.ttft, ri.total, ri.tokens)
				}
				return nil
			case req.Context().Err() != nil:
				return &attemptErr{clientGone: true}
			case u.stalled.Load():
				// Part of the answer is with the caller already, so no other
				// model can finish it. Say so, rather than end as if complete.
				m.noteSlow(u.stall)
				m.recordFailure(failTransient, 0, u.stallMsg(), ri)
				ev, _ := json.Marshal(map[string]any{"error": map[string]any{
					"message": "wend: " + u.stallMsg() + "; send the request again", "type": "upstream_stalled", "code": 504}})
				_, _ = w.Write([]byte("data: " + string(ev) + "\n\n"))
				flush()
				return &attemptErr{committed: true}
			}
			m.recordFailure(failTransient, 0, "stream broke: "+err.Error(), ri)
			return &attemptErr{committed: true}
		}
	}
}

// assembler turns a chat completion stream back into one response.
type assembler struct {
	id, model string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolCall
	finish    string
	usage     json.RawMessage
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// line adds one SSE line. An error event midway is returned as a failure.
func (a *assembler) line(l []byte) *attemptErr {
	ev := sseData(l)
	if ev == nil || string(ev) == "[DONE]" {
		return nil
	}
	if k, status := inspectChunk(ev); k == chunkError {
		kind, pause := classify(status, http.Header{}, ev, &reqInfo{})
		if kind == failClient {
			kind = failTransient // the request was accepted; a later error is the model's
		}
		return &attemptErr{kind: kind, pause: pause, msg: "error in stream: " + string(ev)}
	}
	var c struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Created int64           `json:"created"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          *string `json:"content"`
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(ev, &c) != nil {
		return nil
	}
	if a.id == "" {
		a.id, a.model, a.created = c.ID, c.Model, c.Created
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		a.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		d := ch.Delta
		if d.Content != nil {
			a.content.WriteString(*d.Content)
		}
		if d.ReasoningContent != nil {
			a.reasoning.WriteString(*d.ReasoningContent)
		} else if d.Reasoning != nil {
			a.reasoning.WriteString(*d.Reasoning)
		}
		for _, tc := range d.ToolCalls {
			if a.tools == nil {
				a.tools = map[int]*toolCall{}
			}
			t := a.tools[tc.Index]
			if t == nil {
				t = &toolCall{Type: "function"}
				a.tools[tc.Index] = t
			}
			if tc.ID != "" {
				t.ID = tc.ID
			}
			if tc.Type != "" {
				t.Type = tc.Type
			}
			t.Function.Name += tc.Function.Name
			t.Function.Arguments += tc.Function.Arguments
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finish = *ch.FinishReason
		}
	}
	return nil
}

// empty is a response with no text and no tool call. One cut short by the
// caller's own token limit is not empty: the caller asked for that.
func (a *assembler) empty() bool {
	return a.content.Len() == 0 && len(a.tools) == 0 && a.finish != "length"
}

// tokens is the output size: the provider's count when it sent usage, an
// estimate from the text otherwise.
func (a *assembler) tokens() int {
	chars := a.content.Len() + a.reasoning.Len()
	for _, t := range a.tools {
		chars += len(t.Function.Name) + len(t.Function.Arguments)
	}
	return estimateTokens(chars, completionTokens(a.usage))
}

func estimateTokens(chars, reported int) int {
	if reported > 0 {
		return reported
	}
	return chars / 4
}

// outputChars counts the generated characters in one streamed event, and
// how many of them are reasoning.
func outputChars(ev []byte) (total, reasoning int) {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(ev, &c) != nil {
		return 0, 0
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		reasoning += len(d.Reasoning) + len(d.ReasoningContent)
		total += len(d.Content) + len(d.Reasoning) + len(d.ReasoningContent)
		for _, t := range d.ToolCalls {
			total += len(t.Function.Arguments)
		}
	}
	return total, reasoning
}

func (a *assembler) completion() []byte {
	msg := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		idx := make([]int, 0, len(a.tools))
		for i := range a.tools {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		calls := make([]*toolCall, 0, len(idx))
		for _, i := range idx {
			calls = append(calls, a.tools[i])
		}
		msg["tool_calls"] = calls
		if a.content.Len() == 0 {
			msg["content"] = nil
		}
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{
		"id": a.id, "object": "chat.completion", "created": a.created, "model": a.model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	b, _ := json.Marshal(out)
	return b
}

// cleanMessages drops what some providers reject: fields a client keeps for
// itself (any key starting with "_", which some agents add for themselves), and the name
// field where the model refused it. It returns false when nothing changed.
func cleanMessages(raw json.RawMessage, dropNames bool) (json.RawMessage, bool) {
	if !bytes.Contains(raw, []byte(`"_`)) && !(dropNames && bytes.Contains(raw, []byte(`"name"`))) {
		return nil, false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, false
	}
	for _, m := range msgs {
		for k := range m {
			if strings.HasPrefix(k, "_") || (dropNames && k == "name") {
				delete(m, k)
			}
		}
	}
	out, err := json.Marshal(msgs)
	return out, err == nil
}
