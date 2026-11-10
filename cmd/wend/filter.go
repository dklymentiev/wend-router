package main

// filter.go: masks known sensitive terms (organisations, hosts, people) in
// requests before they leave the router, and restores them in the answer.
// Off by default; switched on from the Filter tab of the dashboard. The
// term list lives in filter.json next to the binary (never in git).
//
// Placeholders look like <ORG_1>. The mapping placeholder -> original is kept
// per session in memory only, and dropped after sessionTTL without traffic.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const filterSessionTTL = 30 * time.Minute

type filterTerm struct {
	term []rune // lower-cased
	typ  string
}

type filterConfig struct {
	Enabled bool   `json:"enabled"`
	Terms   string `json:"terms"`
}

type filterSession struct {
	last     time.Time
	restore  map[string]string // placeholder -> original text
	byValue  map[string]string // lower-cased original -> placeholder
	counters map[string]int    // type -> last number used
	pending  string            // the end of a streamed delta that may be the start of a placeholder
}

type termFilter struct {
	mu       sync.Mutex
	path     string
	cfg      filterConfig
	terms    []filterTerm
	sessions map[string]*filterSession
	cat      *entityCatalog // kinds of values a "@id" line names; nil when the file is broken
	catErr   error
}

type filterSpan struct {
	start, end int
	typ        string
}

func filterPath() string {
	if p := os.Getenv("ROUTER_FILTER"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "filter.json")
	}
	return "filter.json"
}

func newTermFilter(path string) *termFilter {
	f := &termFilter{path: path, sessions: map[string]*filterSession{}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &f.cfg); err != nil {
			log.Printf("filter: %s is not valid JSON, filter stays off: %v", path, err)
			f.cfg = filterConfig{}
		}
	}
	terms, _ := parseFilterTerms(f.cfg.Terms)
	f.terms = terms
	f.cat, f.catErr = loadEntities(entitiesPath())
	if f.catErr != nil {
		log.Printf("filter: %v; requests that use an entity are refused until it is fixed", f.catErr)
	}
	return f
}

// entities resolves the "@id" lines of a term list. An id the catalogue does not
// know is returned in missing: masking must not go ahead without it.
func (f *termFilter) entities(terms []filterTerm) (ents []*entity, missing []string) {
	for _, t := range terms {
		if len(t.term) == 0 || t.term[0] != '@' {
			continue
		}
		id := string(t.term[1:])
		var e *entity
		if f.cat != nil {
			e = f.cat.byID[id]
		}
		if e == nil {
			missing = append(missing, id)
			continue
		}
		ents = append(ents, e)
	}
	return ents, missing
}

// parseFilterTerms reads lines of the form "term<TAB>TYPE". Blank lines and
// lines starting with # are skipped. It returns one error per bad line.
func parseFilterTerms(text string) ([]filterTerm, []string) {
	var out []filterTerm
	var errs []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			errs = append(errs, fmt.Sprintf("line %d: expected term<TAB>TYPE", i+1))
			continue
		}
		typ := strings.ToUpper(strings.TrimSpace(parts[1]))
		term := []rune(strings.ToLower(strings.TrimSpace(parts[0])))
		out = append(out, filterTerm{term: term, typ: typ})
	}
	return out, errs
}

// jsonBytes encodes v as JSON without escaping < > & (json.Marshal would write them as < and so on).
func jsonBytes(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func isCyrillic(r rune) bool {
	return (r >= 'а' && r <= 'я') || r == 'ё'
}

// boundaryAt reports whether position j in lower is a word boundary, where a
// dot followed by a word character does not count (so files.example.lan.org is one name).
func boundaryAt(lower []rune, j int) bool {
	if j >= len(lower) {
		return true
	}
	if isWordRune(lower[j]) {
		return false
	}
	if lower[j] == '.' && j+1 < len(lower) && isWordRune(lower[j+1]) {
		return false
	}
	return true
}

// findFilterSpans finds the listed terms in lower and merges them with the spans
// found by entities (extra): at one start the longer wins, overlaps are dropped.
func findFilterSpans(lower []rune, terms []filterTerm, extra ...filterSpan) []filterSpan {
	spans := append([]filterSpan(nil), extra...)
	for _, t := range terms {
		if len(t.term) == 0 || t.term[0] == '@' { // an entity, found by its patterns
			continue
		}
		n := len(t.term)
		for i := 0; i+n <= len(lower); i++ {
			if lower[i] != t.term[0] || !runesEqual(lower[i:i+n], t.term) {
				continue
			}
			if i > 0 && isWordRune(lower[i-1]) {
				continue
			}
			// Allow a short Russian case ending (up to 3 letters), longest valid wins.
			end := -1
			for k := 0; k <= 3 && i+n+k <= len(lower); k++ {
				if k > 0 && !isCyrillic(lower[i+n+k-1]) {
					break
				}
				if boundaryAt(lower, i+n+k) {
					end = i + n + k
				}
			}
			if end >= 0 {
				spans = append(spans, filterSpan{start: i, end: end, typ: t.typ})
			}
		}
	}
	// Longer spans first at the same start; keep non-overlapping ones.
	sort.Slice(spans, func(a, b int) bool {
		if spans[a].start != spans[b].start {
			return spans[a].start < spans[b].start
		}
		return spans[a].end > spans[b].end
	})
	var out []filterSpan
	last := -1
	for _, s := range spans {
		if s.start >= last {
			out = append(out, s)
			last = s.end
		}
	}
	return out
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lowerRunes(s []rune) []rune {
	out := make([]rune, len(s))
	for i, r := range s {
		out[i] = unicode.ToLower(r)
	}
	return out
}

// maskText replaces every matched term with its placeholder for this session.
func (f *termFilter) maskText(sess *filterSession, text string, terms []filterTerm) string {
	src := []rune(text)
	ents, _ := f.entities(terms) // a missing entity is refused before this, in Request
	spans := findFilterSpans(lowerRunes(src), terms, entitySpans(text, ents)...)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	pos := 0
	for _, s := range spans {
		b.WriteString(string(src[pos:s.start]))
		original := string(src[s.start:s.end])
		key := strings.ToLower(original)
		ph, ok := sess.byValue[key]
		if !ok {
			sess.counters[s.typ]++
			ph = fmt.Sprintf("<%s_%d>", s.typ, sess.counters[s.typ])
			sess.byValue[key] = ph
			sess.restore[ph] = original
		}
		b.WriteString(ph)
		pos = s.end
	}
	b.WriteString(string(src[pos:]))
	return b.String()
}

func (f *termFilter) sessionFor(id string) *filterSession {
	sess, ok := f.sessions[id]
	if !ok {
		sess = &filterSession{restore: map[string]string{}, byValue: map[string]string{}, counters: map[string]int{}}
		f.sessions[id] = sess
	}
	sess.last = time.Now()
	return sess
}

func (f *termFilter) purgeLocked() {
	cutoff := time.Now().Add(-filterSessionTTL)
	for id, s := range f.sessions {
		if s.last.Before(cutoff) {
			delete(f.sessions, id)
		}
	}
}

// Request masks the message texts and the system prompt. Failing here must
// stop the request: the caller returns the error and nothing leaves the router.
func (f *termFilter) Request(session string, body map[string]json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeLocked()
	if !f.cfg.Enabled || len(f.terms) == 0 {
		return nil
	}
	if _, missing := f.entities(f.terms); len(missing) > 0 {
		why := "not in the entity catalogue"
		if f.catErr != nil {
			why = f.catErr.Error()
		}
		return fmt.Errorf("filter: cannot mask %s: %s", strings.Join(missing, ", "), why)
	}
	sess := f.sessionFor(session)
	if raw, ok := body["messages"]; ok {
		var msgs []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return fmt.Errorf("filter: messages is not a JSON array: %w", err)
		}
		for _, m := range msgs {
			if c, ok := m["content"]; ok {
				nc, err := f.maskContent(sess, c)
				if err != nil {
					return err
				}
				m["content"] = nc
			}
		}
		nraw, err := jsonBytes(msgs)
		if err != nil {
			return err
		}
		body["messages"] = nraw
	}
	if raw, ok := body["system"]; ok {
		nc, err := f.maskContent(sess, raw)
		if err != nil {
			return err
		}
		body["system"] = nc
	}
	return nil
}

// maskContent handles content given either as a string or as an array of
// parts with a "text" field (the two shapes both APIs use).
func (f *termFilter) maskContent(sess *filterSession, raw json.RawMessage) (json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return jsonBytes(f.maskText(sess, s, f.terms))
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, nil // unknown shape: leave it, nothing to mask in it
	}
	for _, p := range parts {
		var t string
		if tr, ok := p["text"]; ok && json.Unmarshal(tr, &t) == nil {
			nt, _ := jsonBytes(f.maskText(sess, t, f.terms))
			p["text"] = nt
		}
	}
	return jsonBytes(parts)
}

// Response restores placeholders in a complete (non-streamed) body. The
// original is JSON-escaped, so values with backslashes (paths) stay valid.
func (f *termFilter) Response(session string, body []byte) []byte {
	f.mu.Lock()
	sess, ok := f.sessions[session]
	var restore map[string]string
	if ok {
		restore = make(map[string]string, len(sess.restore))
		for ph, orig := range sess.restore {
			restore[ph] = orig
		}
	}
	f.mu.Unlock()
	if len(restore) == 0 {
		return body
	}
	for ph, orig := range restore {
		esc, _ := jsonBytes(orig)
		esc = esc[1 : len(esc)-1] // the original, escaped as it sits inside a JSON string
		phJSON, _ := jsonBytes(ph)
		phJSON = phJSON[1 : len(phJSON)-1]
		body = bytes.ReplaceAll(body, phJSON, esc)
		body = bytes.ReplaceAll(body, []byte(`<`+ph[1:len(ph)-1]+`>`), esc) // upstreams that escape < and >

	}
	return body
}

// Chunk restores placeholders in a streamed answer's text. A placeholder can come
// split across deltas ("<PER", "SON_1>"), so an end that could still become one is
// held back and sent with the next delta, or when the answer ends. The held end is
// kept per session: two answers streaming at once in one session would share it,
// which a client does not do.
func (f *termFilter) Chunk(session string, line []byte) []byte {
	f.mu.Lock()
	sess, ok := f.sessions[session]
	active := ok && len(sess.restore) > 0
	f.mu.Unlock()
	if !active {
		return line
	}
	body, isData := bytes.CutPrefix(bytes.TrimLeft(line, " "), []byte("data:"))
	if !isData {
		return line
	}
	trimmed := bytes.TrimSpace(body)
	if !bytes.HasPrefix(trimmed, []byte("{")) { // [DONE]: what is held goes first
		return append(f.Flush(session), line...)
	}
	var ev map[string]json.RawMessage
	var choices []map[string]json.RawMessage
	var delta map[string]json.RawMessage
	if json.Unmarshal(trimmed, &ev) != nil || json.Unmarshal(ev["choices"], &choices) != nil || len(choices) == 0 ||
		json.Unmarshal(choices[0]["delta"], &delta) != nil || delta == nil {
		return line
	}
	var text string
	hasText := json.Unmarshal(delta["content"], &text) == nil
	final := len(choices[0]["finish_reason"]) > 0 && string(choices[0]["finish_reason"]) != "null"
	f.mu.Lock()
	if !hasText && (!final || sess.pending == "") {
		f.mu.Unlock()
		return line
	}
	out := f.restoreDelta(sess, text, final)
	f.mu.Unlock()
	delta["content"], _ = jsonBytes(out)
	choices[0]["delta"], _ = jsonBytes(delta)
	ev["choices"], _ = jsonBytes(choices)
	nb, err := jsonBytes(ev)
	if err != nil {
		return line
	}
	ending := line[len(bytes.TrimRight(line, "\r\n")):]
	return append(append([]byte("data: "), nb...), ending...)
}

// restoreDelta joins what was held with the new text, holds back an end that may
// be the start of a placeholder (unless the answer ends here), and restores the rest.
// The caller holds f.mu.
func (f *termFilter) restoreDelta(sess *filterSession, text string, final bool) string {
	s := sess.pending + text
	sess.pending = ""
	if i := strings.LastIndexByte(s, '<'); !final && i >= 0 && len(s)-i <= 40 && placeholderStart(s[i:]) {
		s, sess.pending = s[:i], s[i:]
	}
	return restoreText(sess, s)
}

// placeholderStart says whether s could grow into a placeholder such as <ORG_12>.
func placeholderStart(s string) bool {
	for _, c := range s[1:] {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Flush releases a held end when a stream finishes without a final delta.
func (f *termFilter) Flush(session string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[session]
	if !ok || sess.pending == "" {
		return nil
	}
	out := restoreText(sess, sess.pending)
	sess.pending = ""
	b, _ := jsonBytes(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": out}}}})
	return append(append([]byte("data: "), b...), "\n\n"...)
}

// handleFilter serves the Filter tab. GET returns the current state; PUT
// stores a new term list and switch position, and reports bad lines.
func (r *Router) handleFilter(w http.ResponseWriter, req *http.Request) {
	f := r.filter
	if req.Method == "PUT" {
		var body filterConfig
		if err := readJSON(req, &body); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		terms, errs := parseFilterTerms(body.Terms)
		if _, missing := f.entities(terms); len(missing) > 0 {
			errs = append(errs, "not in the entity catalogue: "+strings.Join(missing, ", "))
		}
		if len(errs) > 0 {
			writeError(w, 400, strings.Join(errs, "; "), "invalid_request_error")
			return
		}
		f.mu.Lock()
		f.cfg = body
		f.terms = terms
		raw, err := json.MarshalIndent(f.cfg, "", "  ")
		if err == nil {
			tmp := f.path + ".tmp"
			err = os.WriteFile(tmp, raw, 0o600)
			if err == nil {
				err = os.Rename(tmp, f.path)
			}
		}
		f.mu.Unlock()
		if err != nil {
			writeError(w, 500, "could not save filter: "+err.Error(), "server_error")
			return
		}
		log.Printf("filter: enabled=%v, %d terms", body.Enabled, len(terms))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	type kind struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	var kinds []kind
	if f.cat != nil {
		for _, e := range f.cat.Entities {
			kinds = append(kinds, kind{e.ID, e.Name, e.Type})
		}
	}
	catErr := ""
	if f.catErr != nil {
		catErr = f.catErr.Error()
	}
	writeJSON(w, map[string]any{"enabled": f.cfg.Enabled, "terms": f.cfg.Terms, "count": len(f.terms),
		"entities": kinds, "entities_file": entitiesPath(), "entities_error": catErr})
}

// handleFilterTry shows what a model would get for a text under the saved terms,
// on or off: the dashboard's "Try it". Nothing is kept.
func (r *Router) handleFilterTry(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(req, &body); err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	f := r.filter
	f.mu.Lock()
	terms := f.terms
	f.mu.Unlock()
	sess := &filterSession{restore: map[string]string{}, byValue: map[string]string{}, counters: map[string]int{}}
	writeJSON(w, map[string]any{"masked": f.maskText(sess, body.Text, terms)})
}

// restoreText puts the originals back in a plain text answer.
func restoreText(sess *filterSession, text string) string {
	for ph, orig := range sess.restore {
		text = strings.ReplaceAll(text, ph, orig)
	}
	return text
}

// handleFilterAsk is the round trip, for the dashboard's test panel: the text is
// masked with the saved terms, sent to a model (the given one, or the one "auto"
// would pick), and the answer comes back as the model wrote it and restored.
// It masks whether or not the filter is switched on: it shows what the filter does.
func (r *Router) handleFilterAsk(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := readJSON(req, &body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeError(w, 400, "nothing to send", "invalid_request_error")
		return
	}
	var m *Model
	if body.Model == "" || body.Model == "auto" {
		if c := r.candidates(&reqInfo{model: "auto", outTokens: defaultOutTokens}); len(c) > 0 {
			m = c[0]
		}
	} else {
		r.mu.RLock()
		for _, x := range r.models {
			if x.Key == body.Model {
				m = x
			}
		}
		r.mu.RUnlock()
	}
	if m == nil {
		writeError(w, 404, "no model to ask", "invalid_request_error")
		return
	}
	f := r.filter
	f.mu.Lock()
	terms := f.terms
	f.mu.Unlock()
	sess := &filterSession{restore: map[string]string{}, byValue: map[string]string{}, counters: map[string]int{}}
	masked := f.maskText(sess, body.Text, terms)
	msgs, _ := json.Marshal([]map[string]string{{"role": "user", "content": masked}})
	a, ae := r.call(req.Context(), m, "filter", map[string]json.RawMessage{"messages": msgs, "max_tokens": json.RawMessage("800")})
	if ae != nil {
		writeJSON(w, map[string]any{"model": m.Key, "masked": masked, "error": ae.kind.String() + ": " + truncate(oneLine(ae.msg), 200)})
		return
	}
	writeJSON(w, map[string]any{"model": m.Key, "masked": masked, "answer": a.Text, "restored": restoreText(sess, a.Text)})
}
