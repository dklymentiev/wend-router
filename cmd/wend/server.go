package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// reqInfo is what routing needs to know about an incoming request.
type reqInfo struct {
	model        string
	lastChance   bool // no other model is left to try after this attempt
	stream       bool
	hasTools     bool
	hasImage     bool
	hasNames     bool // some message carries a name field
	estTokens    int  // rough size, prompt plus requested output
	promptTokens int  // estimated, replaced by the provider's count when it reports one
	client       string
	// promptReported and outReported say the provider gave the token
	// counts; otherwise they are the router's estimates.
	promptReported, outReported bool
	outTokens                   int // max_tokens, or a default, for the cost estimate
	session                     string
	id                          string // ties the traffic log events of one request together
	body                        map[string]json.RawMessage

	// Filled by the attempt that served the request.
	ttft, total  time.Duration
	tokens       int
	cost         float64  // USD
	costSource   string   // free, provider (reported in the response) or estimate
	reportedCost *float64 // USD, from the response's usage.cost
	probe        bool     // sent by the prober, not a client
	label        string   // what sent it when not a client, for the pipe: "check", or "test: <case>"
}

func parseRequest(req *http.Request, raw []byte) (*reqInfo, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	ri := &reqInfo{body: body, session: strings.TrimSpace(req.Header.Get("X-Session-Id"))}
	_ = json.Unmarshal(body["model"], &ri.model)
	_ = json.Unmarshal(body["stream"], &ri.stream)
	if t := bytes.TrimSpace(body["tools"]); len(t) > 0 && string(t) != "null" && string(t) != "[]" {
		ri.hasTools = true
	}
	msgs := body["messages"]
	ri.hasImage = bytes.Contains(msgs, []byte(`"image_url"`)) || bytes.Contains(msgs, []byte(`"input_image"`))
	ri.hasNames = bytes.Contains(msgs, []byte(`"name":`))
	ri.promptTokens = promptEstimate(msgs, body["tools"])
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		ri.outTokens = max(ri.outTokens, jsonInt(body[k]))
	}
	ri.estTokens = ri.promptTokens + ri.outTokens
	if ri.outTokens == 0 {
		ri.outTokens = defaultOutTokens
	}
	return ri, nil
}

// attemptErr describes a failed attempt on one model.
type attemptErr struct {
	kind       failKind
	pause      time.Duration
	status     int
	body       []byte
	msg        string
	clientGone bool // the caller hung up: stop, blame nobody
	committed  bool // bytes already reached the caller: no fallback possible
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", r.auth(r.handleChat))
	mux.HandleFunc("POST /v1/messages", r.auth(r.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", r.auth(r.handleCountTokens))
	mux.HandleFunc("GET /v1/models", r.auth(r.handleModels))
	mux.HandleFunc("GET /status.json", r.handleStatusJSON)
	mux.HandleFunc("GET /api/usage", r.handleUsage)
	mux.HandleFunc("GET /status", r.handleStatusHTML)
	mux.HandleFunc("GET /health", r.handleHealth)
	mux.HandleFunc("GET /events", r.handleEvents)
	mux.HandleFunc("GET /pipe", r.handlePipe)
	mux.HandleFunc("GET /api/profiles", r.handleProfiles)
	mux.HandleFunc("PUT /api/profiles/{name}", r.ui(r.handleProfilePut))
	mux.HandleFunc("DELETE /api/profiles/{name}", r.ui(r.handleProfileDelete))
	mux.HandleFunc("GET /api/providers", r.handleProviders)
	mux.HandleFunc("POST /api/providers", r.ui(r.handleCustomAdd))
	mux.HandleFunc("DELETE /api/providers/{id}", r.ui(r.handleProviderDelete))
	mux.HandleFunc("POST /api/providers/{id}/keys", r.ui(r.handleKeyAdd))
	mux.HandleFunc("DELETE /api/providers/{id}/keys/{kid}", r.ui(r.handleKeyDelete))
	mux.HandleFunc("POST /api/providers/{id}/vars", r.ui(r.handleVars))
	mux.HandleFunc("POST /api/providers/{id}/enabled", r.ui(r.handleEnabled))
	mux.HandleFunc("POST /api/preview", r.handlePreview)
	mux.HandleFunc("POST /api/probe", r.ui(r.handleProbe))
	mux.HandleFunc("POST /api/test", r.ui(r.handleTest))
	mux.HandleFunc("POST /api/test/stop", r.ui(r.handleTestStop))
	mux.HandleFunc("GET /api/filter", r.handleFilter)
	mux.HandleFunc("PUT /api/filter", r.ui(r.handleFilter))
	mux.HandleFunc("POST /api/filter/try", r.ui(r.handleFilterTry))
	mux.HandleFunc("POST /api/filter/ask", r.ui(r.handleFilterAsk))
	mux.HandleFunc("GET /api/traffic", r.handleTraffic)
	mux.HandleFunc("POST /api/traffic", r.ui(r.handleTraffic))
	mux.HandleFunc("GET /dashboard", r.handleSimple)
	mux.HandleFunc("GET /pro", r.handleDashboard)
	mux.HandleFunc("GET /ui/kit.css", r.handleKit)
	mux.HandleFunc("GET /ui/help.js", r.handleHelpJS)
	mux.HandleFunc("GET /help", r.handleHelp)
	mux.HandleFunc("GET /help/{page}", r.handleHelp)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "/dashboard", http.StatusFound) })
	return mux
}

func (r *Router) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.cfg.Token != "" && req.Header.Get("Authorization") != "Bearer "+r.cfg.Token && req.Header.Get("X-Api-Key") != r.cfg.Token {
			writeError(w, 401, "missing or wrong router token", "unauthorized")
			return
		}
		next(w, req)
	}
}

func (r *Router) handleChat(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	id := newRequestID()
	if r.pipe.on() {
		r.pipe.send(pipeEvent{ID: id, Kind: "client_request"}, raw, pipeTextCap)
		pw := &pipeWriter{ResponseWriter: w, pipe: r.pipe, id: id}
		w = pw
		defer func() {
			r.pipe.send(pipeEvent{ID: id, Kind: "client_end", Status: pw.status, Model: pw.Header().Get("X-Router-Model")}, nil, 0)
		}()
	}
	if r.traffic.on.Load() {
		r.traffic.write(map[string]any{"id": id, "kind": "client_request", "body": jsonOrText(raw)})
		rec := &recorder{ResponseWriter: w}
		w = rec
		defer func() {
			r.traffic.write(map[string]any{"id": id, "kind": "client_response", "status": rec.status,
				"model": rec.Header().Get("X-Router-Model"), "body": jsonOrText(rec.buf.Bytes())})
		}()
	}
	ri, err := parseRequest(req, raw)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	ri.id = id
	// A client that names no session gets one from the conversation itself.
	client := clientName(req)
	ri.client = client
	explicit := ri.session != ""
	if !explicit {
		ri.session = fingerprint(client, ri.model, ri.body["messages"])
	}
	r.book.start(ri.session, client, nonEmptyOr(ri.model, "auto"), explicit)
	if err := r.applyRequest(ri.session, ri.body); err != nil {
		writeError(w, 500, err.Error(), "router_error")
		return
	}
	cands := r.candidates(ri)
	if len(cands) == 0 {
		cands = r.waitForModel(req.Context(), ri)
	}
	if len(cands) == 0 {
		if r.unknown(ri.model) {
			writeError(w, 404, fmt.Sprintf("wend has no profile or model named %q: ask for a profile (auto, fast, tools, ...) or a model listed at /v1/models", ri.model), "model_not_found")
			return
		}
		writeError(w, 503, fmt.Sprintf("none of the models %q takes can answer now", nonEmptyOr(ri.model, "auto")), "router_exhausted")
		return
	}
	var (
		tried      []string
		failures   []string
		last       *attemptErr
		clientErrs = map[string]bool{}
	)
	// fail books a failed attempt; true means stop trying other models.
	fail := func(m *Model, ae *attemptErr) bool {
		if ae.kind == failAuth {
			m.Provider.refuseKey(truncate(ae.msg, 120))
		}
		m.recordFailure(ae.kind, ae.pause, ae.msg, ri)
		log.Printf("%s: %s %s", m.Key, ae.kind, truncate(oneLine(ae.msg), 160))
		failures = append(failures, m.Key+" ("+ae.kind.String()+")")
		last = ae
		// The same 400 from two providers means the request is at fault.
		if ae.kind == failClient {
			clientErrs[m.Provider.Spec.ID] = true
			if len(clientErrs) >= 2 {
				r.logRequest(ri, "", "client-error", tried, failures)
				writeUpstream(w, ae)
				return true
			}
		}
		return false
	}
	hedge := r.hedgeAfter(ri)
	for i, m := range cands {
		if containsKey(tried, m.Key) {
			continue // already raced as a backup
		}
		if len(tried) >= r.cfg.MaxAttempts {
			break
		}
		tried = append(tried, m.Key)
		ri.lastChance = i == len(cands)-1 || len(tried) == r.cfg.MaxAttempts
		var backup *Model
		if hedge > 0 && !ri.lastChance {
			backup = pickBackup(m, cands[i+1:], tried)
		}
		var ae *attemptErr
		var lost []lostAttempt
		m, ae, tried, lost = r.attempt(w, req, ri, m, backup, hedge, tried)
		for _, l := range lost {
			if fail(l.m, l.ae) {
				return
			}
		}
		if ae == nil {
			r.settle(m, ri)
			r.remember(stickyKey(ri), m.Key)
			r.logRequest(ri, m.Key, "ok", tried, failures)
			return
		}
		if ae.clientGone || ae.committed {
			outcome := "client-left"
			if ae.committed {
				outcome = "broke-midway"
			}
			r.logRequest(ri, m.Key, outcome, tried, failures)
			return
		}
		if fail(m, ae) {
			return
		}
	}
	r.logRequest(ri, "", "failed", tried, failures)
	if last != nil && last.kind == failClient {
		writeUpstream(w, last)
		return
	}
	writeError(w, 503, "all models failed: "+strings.Join(failures, ", "), "router_exhausted")
}

func netFailure(req *http.Request, err error) *attemptErr {
	if req.Context().Err() != nil {
		return &attemptErr{clientGone: true}
	}
	return &attemptErr{kind: failTransient, msg: err.Error()}
}

// sseData returns the payload of a "data:" line, or nil for anything else.
func sseData(line []byte) []byte {
	line = bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	return bytes.TrimSpace(line[5:])
}

type chunkKind int

const (
	chunkNone  chunkKind = iota // role header, empty delta, usage, keep-alive
	chunkToken                  // content, reasoning or a tool call
	chunkError
)

// inspectChunk says whether a streamed event carries a real token or an
// error. For errors it also returns an HTTP-like status from the error code.
func inspectChunk(ev []byte) (chunkKind, int) {
	var c struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(ev, &c) != nil {
		return chunkNone, 0
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return chunkError, errorStatus(c.Error)
	}
	for _, ch := range c.Choices {
		for k, v := range ch.Delta {
			switch k {
			case "content", "reasoning", "reasoning_content":
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return chunkToken, 0
				}
			case "tool_calls":
				if t := string(bytes.TrimSpace(v)); t != "null" && t != "[]" {
					return chunkToken, 0
				}
			}
		}
	}
	return chunkNone, 0
}

// embeddedError finds an {"error": ...} object sent with a 200.
func embeddedError(body []byte) (int, bool) {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error) == 0 || string(e.Error) == "null" {
		return 0, false
	}
	return errorStatus(e.Error), true
}

func errorStatus(raw json.RawMessage) int {
	var e struct {
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if n := jsonInt(e.Code); n >= 400 && n < 600 {
			return n
		}
	}
	return 500
}

// emptyCompletion reports a 200 whose first choice has neither text nor a
// tool call: a billed nothing, treated as a failure.
func emptyCompletion(body []byte) bool {
	var c struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &c) != nil || len(c.Choices) == 0 {
		return true
	}
	msg := c.Choices[0].Message
	if t := string(bytes.TrimSpace(msg["tool_calls"])); t != "" && t != "null" && t != "[]" {
		return false
	}
	content := bytes.TrimSpace(msg["content"])
	switch string(content) {
	case "", "null", `""`, "[]":
		return true
	}
	return false
}

func setRouterHeaders(w http.ResponseWriter, m *Model, tried []string) {
	w.Header().Set("X-Router-Model", m.Key)
	w.Header().Set("X-Router-Attempts", strconv.Itoa(len(tried)))
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": status},
	})
}

func writeUpstream(w http.ResponseWriter, ae *attemptErr) {
	status := ae.status
	if status == 0 {
		status = 400
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(ae.body)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (r *Router) handleModels(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var data []entry
	for _, n := range r.profileNames() {
		data = append(data, entry{n, "model", "wend"})
	}
	for _, s := range r.snapshot() {
		data = append(data, entry{s.Key, "model", s.Provider})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (r *Router) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snap := r.snapshot()
	usable := 0
	for _, s := range snap {
		if s.State == "ok" {
			usable++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": usable > 0, "models": len(snap), "usable": usable, "version": version})
}

func (r *Router) handleStatusJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"time": time.Now().Format(time.RFC3339), "models": r.snapshot()})
}

var statusPage = template.Must(template.New("status").Funcs(template.FuncMap{
	"pct": func(f float64) string {
		if f < 0 {
			return ""
		}
		return fmt.Sprintf("%.0f%%", f*100)
	},
}).Parse(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>wend</title>
<style>body{font:13px system-ui,sans-serif;margin:20px}table{border-collapse:collapse}
td,th{padding:4px 10px;border-bottom:1px solid #ddd;text-align:left}
.ok{color:#1a7f37}.quota,.open{color:#9a6700}.gone,.provider-off{color:#cf222e}</style>
<h2>wend</h2><p>{{.Time}} · refreshes every 10 s</p>
<table><tr><th>Tier</th><th>Model</th><th>State</th><th>TTFT ms</th><th>Total ms</th><th>Avail</th><th>OK</th><th>Err</th><th>Ctx</th><th>Tools</th><th>Vision</th><th>Last error</th></tr>
{{range .Models}}<tr><td>{{.Tier}}</td><td>{{.Key}}{{if .TrainsOnFree}} *{{end}}</td><td class="{{.State}}">{{.State}}{{if .Until}} until {{.Until}}{{end}}</td>
<td>{{if .TTFTMs}}{{.TTFTMs}}{{end}}</td><td>{{if .TotalMs}}{{.TotalMs}}{{end}}</td><td>{{pct .Availability}}</td><td>{{.OK}}</td><td>{{.Errors}}</td>
<td>{{if .Context}}{{.Context}}{{end}}</td><td>{{.Tools}}</td><td>{{.Vision}}</td><td>{{.LastError}}</td></tr>{{end}}
</table><p>* provider's free tier may use prompts for training</p>`))

func (r *Router) handleStatusHTML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = statusPage.Execute(w, map[string]any{"Time": time.Now().Format("2006-01-02 15:04:05"), "Models": r.snapshot()})
}
