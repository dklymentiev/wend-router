package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// Anthropic Messages API (POST /v1/messages), for agents that speak only
// Claude's API. The request is translated to a chat completion, routed like
// any other, and the answer translated back, streaming included. A model
// name the router does not know (claude-sonnet-...) picks the profile in
// ROUTER_ANTHROPIC_PROFILE, "auto" by default.

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result: string or blocks
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"` // base64 or url
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or blocks
}

type anthRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []anthMessage   `json:"messages"`
	Tools     []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
	Stream        bool     `json:"stream"`
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"top_p"`
	StopSequences []string `json:"stop_sequences"`
}

// blocks reads content that is either a string or a list of blocks.
func blocks(raw json.RawMessage) []anthBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anthBlock{{Type: "text", Text: s}}
	}
	var bs []anthBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func textOf(raw json.RawMessage) string {
	var parts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toOpenAI turns an Anthropic request into a chat completion body.
func (r *Router) toOpenAI(a *anthRequest) ([]byte, error) {
	var msgs []map[string]any
	if sys := textOf(a.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range a.Messages {
		bs := blocks(m.Content)
		switch m.Role {
		case "assistant":
			msg := map[string]any{"role": "assistant"}
			var text []string
			var calls []map[string]any
			for _, b := range bs {
				switch b.Type {
				case "text":
					text = append(text, b.Text)
				case "tool_use":
					args := string(b.Input)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": args}})
				}
			}
			msg["content"] = strings.Join(text, "\n")
			if len(calls) > 0 {
				msg["tool_calls"] = calls
				if len(text) == 0 {
					msg["content"] = nil
				}
			}
			msgs = append(msgs, msg)
		default: // user
			var parts []map[string]any
			flush := func() {
				if len(parts) > 0 {
					msgs = append(msgs, map[string]any{"role": "user", "content": parts})
					parts = nil
				}
			}
			for _, b := range bs {
				switch b.Type {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				case "image":
					if b.Source == nil {
						continue
					}
					url := b.Source.URL
					if b.Source.Type == "base64" {
						url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				case "tool_result":
					flush() // tool results are their own messages, in order
					content := textOf(b.Content)
					if b.IsError {
						content = "Error: " + content
					}
					msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
				}
			}
			flush()
		}
	}
	model := a.Model
	if r.profile(model) == nil && !r.knownModel(model) {
		model = envOr("ROUTER_ANTHROPIC_PROFILE", "auto")
	}
	out := map[string]any{"model": model, "messages": msgs, "stream": a.Stream}
	if a.MaxTokens > 0 {
		out["max_tokens"] = a.MaxTokens
	}
	if a.Temperature != nil {
		out["temperature"] = *a.Temperature
	}
	if a.TopP != nil {
		out["top_p"] = *a.TopP
	}
	if len(a.StopSequences) > 0 {
		out["stop"] = a.StopSequences
	}
	if len(a.Tools) > 0 {
		var tools []map[string]any
		for _, t := range a.Tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
		}
		out["tools"] = tools
		if tc := a.ToolChoice; tc != nil {
			switch tc.Type {
			case "any":
				out["tool_choice"] = "required"
			case "none":
				out["tool_choice"] = "none"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
			}
		}
	}
	return json.Marshal(out)
}

func (r *Router) knownModel(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.Key == name || m.ID == name {
			return true
		}
	}
	return false
}

func stopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// toAnthropic turns a completed chat completion into a Messages response.
func toAnthropic(body []byte, id, model string) []byte {
	var c struct {
		Choices []struct {
			Message struct {
				Content   *string     `json:"content"`
				ToolCalls []*toolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &c)
	content := []map[string]any{}
	finish := ""
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		finish = ch.FinishReason
		if ch.Message.Content != nil && *ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": *ch.Message.Content})
		}
		for _, t := range ch.Message.ToolCalls {
			input := json.RawMessage(t.Function.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Function.Name, "input": input})
		}
		if len(ch.Message.ToolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	out, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": stopReason(finish), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens},
	})
	return out
}

func anthError(w http.ResponseWriter, status int, msg string) {
	typ := map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 429: "rate_limit_error", 503: "overloaded_error"}[status]
	if typ == "" {
		typ = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func (r *Router) handleMessages(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	if err != nil {
		anthError(w, 400, err.Error())
		return
	}
	var a anthRequest
	if err := json.Unmarshal(raw, &a); err != nil {
		anthError(w, 400, "request body is not a Messages API object: "+err.Error())
		return
	}
	payload, err := r.toOpenAI(&a)
	if err != nil {
		anthError(w, 400, err.Error())
		return
	}
	inner := req.Clone(req.Context())
	inner.Body = io.NopCloser(bytes.NewReader(payload))
	inner.ContentLength = int64(len(payload))
	id := "msg_" + newRequestID()

	if !a.Stream {
		rec := httptest.NewRecorder()
		r.handleChat(rec, inner)
		for k, v := range rec.Header() {
			if strings.HasPrefix(k, "X-Router") {
				w.Header()[k] = v
			}
		}
		if rec.Code != 200 {
			var e struct {
				Error struct{ Message string } `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			anthError(w, rec.Code, nonEmptyOr(e.Error.Message, rec.Body.String()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(toAnthropic(rec.Body.Bytes(), id, rec.Header().Get("X-Router-Model")))
		return
	}
	sw := &anthStream{w: w, id: id}
	r.handleChat(sw, inner)
	sw.finish()
}

// anthStream sits where handleChat writes its OpenAI stream and writes
// Anthropic events instead. An error before the stream starts goes out as
// an Anthropic error.
type anthStream struct {
	w      http.ResponseWriter
	id     string
	status int
	errBuf bytes.Buffer
	line   bytes.Buffer
	model  string

	started      bool
	open         string // type of the open content block: "", "text" or "tool_use"
	index        int    // index of the open block
	toolIdx      map[int]int
	finishReason string
	inTokens     int
	outToks      int
	ended        bool
}

func (s *anthStream) Header() http.Header { return s.w.Header() }

func (s *anthStream) WriteHeader(code int) {
	s.status = code
	if code == 200 {
		s.model = s.w.Header().Get("X-Router-Model")
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.WriteHeader(200)
	}
}

func (s *anthStream) Flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *anthStream) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.WriteHeader(200)
	}
	if s.status != 200 {
		s.errBuf.Write(p)
		return len(p), nil
	}
	s.line.Write(p)
	for {
		b := s.line.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := string(b[:i])
		s.line.Next(i + 1)
		s.handle(line)
	}
	return len(p), nil
}

func (s *anthStream) event(name string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
}

func (s *anthStream) start() {
	if s.started {
		return
	}
	s.started = true
	s.toolIdx = map[int]int{}
	s.index = -1
	s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (s *anthStream) closeBlock() {
	if s.open != "" {
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
		s.open = ""
	}
}

func (s *anthStream) handle(line string) {
	ev := sseData([]byte(line))
	if s.ended || ev == nil || string(ev) == "[DONE]" {
		return
	}
	var c struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(ev, &c) != nil {
		return
	}
	// An error midway ends the answer as an error, not as a finished one.
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(ev, &e) == nil && e.Error != nil && len(c.Choices) == 0 {
		s.start()
		s.closeBlock()
		s.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": e.Error.Message}})
		s.ended = true
		s.Flush()
		return
	}
	s.start()
	if c.Usage != nil {
		s.inTokens, s.outToks = c.Usage.PromptTokens, c.Usage.CompletionTokens
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.Content != "" {
			if s.open != "text" {
				s.closeBlock()
				s.index++
				s.open = "text"
				s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": map[string]any{"type": "text", "text": ""}})
			}
			s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index, "delta": map[string]any{"type": "text_delta", "text": d.Content}})
		}
		for _, tc := range d.ToolCalls {
			idx, seen := s.toolIdx[tc.Index]
			if !seen {
				s.closeBlock()
				s.index++
				idx = s.index
				s.toolIdx[tc.Index] = idx
				s.open = "tool_use"
				s.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{
					"type": "tool_use", "id": nonEmptyOr(tc.ID, fmt.Sprintf("toolu_%s_%d", s.id, tc.Index)), "name": tc.Function.Name, "input": map[string]any{}}})
			}
			if tc.Function.Arguments != "" {
				s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finishReason = *ch.FinishReason
		}
	}
	s.Flush()
}

// finish closes the stream, or sends the error if it never started.
func (s *anthStream) finish() {
	if s.status != 200 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(s.errBuf.Bytes(), &e)
		anthError(s.w, s.status, nonEmptyOr(e.Error.Message, s.errBuf.String()))
		return
	}
	if s.ended {
		return
	}
	s.ended = true
	s.start()
	s.closeBlock()
	if len(s.toolIdx) > 0 {
		s.finishReason = "tool_calls"
	}
	s.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason(s.finishReason), "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": s.inTokens, "output_tokens": s.outToks}})
	s.event("message_stop", map[string]any{"type": "message_stop"})
	s.Flush()
}

// handleCountTokens estimates input tokens, for clients that ask first.
func (r *Router) handleCountTokens(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	writeJSON(w, map[string]int{"input_tokens": len(raw) / 4})
}
