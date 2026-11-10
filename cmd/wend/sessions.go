package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sessions: one conversation of one client. A client that sends no
// X-Session-Id is recognised by a fingerprint of the conversation (system
// prompt and first user message, which every step of a task repeats), so
// the steps of one task stay on one model while it answers, and the
// dashboard can show what each client is doing.

const sessionKeep = 24 * time.Hour

type sessionInfo struct {
	ID        string         `json:"id"`
	Client    string         `json:"client"`
	Profile   string         `json:"profile"`
	First     time.Time      `json:"first"`
	Last      time.Time      `json:"last"`
	Requests  int            `json:"requests"`
	InFlight  int            `json:"inflight"`
	Fallbacks int            `json:"fallbacks"` // requests that needed more than one model
	Failed    int            `json:"failed"`    // requests no model could serve
	TokensIn  int            `json:"tokens_in"`
	TokensOut int            `json:"tokens_out"`
	Current   string         `json:"current,omitempty"`
	Models    map[string]int `json:"models"` // model -> requests it served
	Switches  []string       `json:"switches,omitempty"`
	Explicit  bool           `json:"explicit"` // the client sent X-Session-Id
}

type sessionBook struct {
	mu   sync.Mutex
	byID map[string]*sessionInfo
}

func newSessionBook() *sessionBook { return &sessionBook{byID: map[string]*sessionInfo{}} }

// clientName says who is calling: X-Router-Client, else the user agent's
// first word.
func clientName(req *http.Request) string {
	if c := strings.TrimSpace(req.Header.Get("X-Router-Client")); c != "" {
		return truncate(c, 40)
	}
	ua := strings.TrimSpace(req.Header.Get("User-Agent"))
	if ua == "" {
		return "unknown"
	}
	if i := strings.IndexAny(ua, " ;("); i > 0 {
		ua = ua[:i]
	}
	return truncate(ua, 40)
}

// fingerprint names a conversation by what all its steps share.
func fingerprint(client, profile string, msgs json.RawMessage) string {
	var list []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(msgs, &list)
	h := sha256.New()
	h.Write([]byte(client + "\x00" + profile + "\x00"))
	userSeen := false
	for _, m := range list {
		c := m.Content
		if len(c) > 4096 {
			c = c[:4096]
		}
		switch {
		case m.Role == "system" || m.Role == "developer":
			h.Write(c)
		case m.Role == "user" && !userSeen:
			h.Write(c)
			userSeen = true
		}
		if userSeen {
			break
		}
	}
	return "fp-" + hex.EncodeToString(h.Sum(nil))[:12]
}

func (b *sessionBook) start(id, client, profile string, explicit bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	s := b.byID[id]
	if s == nil {
		s = &sessionInfo{ID: id, Client: client, Profile: profile, First: now, Last: now, Models: map[string]int{}, Explicit: explicit}
		b.byID[id] = s
		for k, v := range b.byID { // forget old sessions as new ones come
			if now.Sub(v.Last) > sessionKeep && v.InFlight == 0 {
				delete(b.byID, k)
			}
		}
	}
	s.Last = now
	s.Requests++
	s.InFlight++
}

// finish records how a request of the session ended.
func (b *sessionBook) finish(id, served string, failures []string, tokensIn, tokensOut int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.byID[id]
	if s == nil {
		return
	}
	s.InFlight--
	s.Last = time.Now()
	s.TokensIn += tokensIn
	s.TokensOut += tokensOut
	if len(failures) > 0 {
		s.Fallbacks++
		sw := clock(s.Last) + " " + strings.Join(failures, ", ")
		if served != "" {
			sw += ", then " + served
		} else {
			sw += ", nothing answered"
		}
		s.Switches = append(s.Switches, sw)
		if len(s.Switches) > 8 {
			s.Switches = s.Switches[len(s.Switches)-8:]
		}
	}
	if served == "" {
		s.Failed++
		return
	}
	if s.Current != "" && s.Current != served && len(failures) == 0 {
		s.Switches = append(s.Switches, clock(s.Last)+" moved "+s.Current+" to "+served)
	}
	s.Current = served
	s.Models[served]++
}

func clock(t time.Time) string { return t.Format("15:04:05") }

// clientInfo is one calling tool over a window: what the simple dashboard lists.
type clientInfo struct {
	Client   string    `json:"client"`
	Profiles []string  `json:"profiles"` // model names it asked for, most used first
	Requests int       `json:"requests"`
	InFlight int       `json:"inflight"`
	Last     time.Time `json:"last"`
}

// clients sums the sessions of the last window by client, latest first.
func (b *sessionBook) clients(window time.Duration) []clientInfo {
	by := map[string]*clientInfo{}
	asked := map[string]map[string]int{}
	for _, s := range b.active(window) {
		c := by[s.Client]
		if c == nil {
			c = &clientInfo{Client: s.Client}
			by[s.Client], asked[s.Client] = c, map[string]int{}
		}
		c.Requests += s.Requests
		c.InFlight += s.InFlight
		if s.Last.After(c.Last) {
			c.Last = s.Last
		}
		asked[s.Client][s.Profile] += s.Requests
	}
	out := make([]clientInfo, 0, len(by))
	for name, c := range by {
		for p := range asked[name] {
			c.Profiles = append(c.Profiles, p)
		}
		sort.Slice(c.Profiles, func(i, j int) bool { return asked[name][c.Profiles[i]] > asked[name][c.Profiles[j]] })
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// active lists sessions seen in the last window, newest first.
func (b *sessionBook) active(window time.Duration) []sessionInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var out []sessionInfo
	for _, s := range b.byID {
		if now.Sub(s.Last) <= window || s.InFlight > 0 {
			c := *s
			c.Models = make(map[string]int, len(s.Models))
			for k, v := range s.Models {
				c.Models[k] = v
			}
			c.Switches = append([]string(nil), s.Switches...)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}
