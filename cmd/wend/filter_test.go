package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testTerms(t *testing.T, text string) []filterTerm {
	t.Helper()
	terms, errs := parseFilterTerms(text)
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	return terms
}

func TestFilterMatchesWholeWordsAndCaseEndings(t *testing.T) {
	terms := testTerms(t, "Acme\tORG\nfiles.example.lan\tHOST\nmerlin\tHOST\n")
	cases := []struct {
		text string
		want []string // matched substrings
	}{
		{"Письмо Acme отправлено", []string{"Acme"}},
		{"ответ от acme пришёл", []string{"acme"}},
		{"Acmeosaurus is a dinosaur", nil},           // no match inside a longer word
		{"merlinу не отвечает", []string{"merlinу"}}, // Russian case ending
		{"files.example.lan.org", nil},               // dot continues the name
		{"ssh files.example.lan", []string{"files.example.lan"}},
	}
	for _, c := range cases {
		spans := findFilterSpans(lowerRunes([]rune(c.text)), terms)
		var got []string
		runes := []rune(c.text)
		for _, s := range spans {
			got = append(got, string(runes[s.start:s.end]))
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %v, want %v", c.text, got, c.want)
		}
	}
}

func TestFilterRoundTripKeepsJSONValid(t *testing.T) {
	terms := testTerms(t, "Acme\tORG\nC:\\Users\\jdoe\tPATH_HEAD\n")
	f := &termFilter{sessions: map[string]*filterSession{}, terms: terms,
		cfg: filterConfig{Enabled: true, Terms: "x"}}
	body := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"Письмо Acme, файл C:\\Users\\jdoe\\a.txt"}]`),
	}
	if err := f.Request("s1", body); err != nil {
		t.Fatal(err)
	}
	masked := string(body["messages"])
	if strings.Contains(masked, "Acme") || strings.Contains(masked, "jdoe") {
		t.Fatalf("sensitive value left in request: %s", masked)
	}
	if !strings.Contains(masked, "<ORG_1>") || !strings.Contains(masked, "<PATH_HEAD_1>") {
		t.Fatalf("placeholders missing: %s", masked)
	}
	// A model answer that reuses the placeholders, with the path escaped the way JSON does it.
	answer := []byte(`{"content":"Ответ для <ORG_1>, файл <PATH_HEAD_1>\\a.txt"}`)
	restored := string(f.Response("s1", answer))
	if !strings.Contains(restored, "Acme") || !strings.Contains(restored, `C:\\Users\\jdoe\\a.txt`) {
		t.Fatalf("restore failed: %s", restored)
	}
	// Upstreams that escape < and > must restore too.
	escaped := []byte(`{"content":"for <ORG_1>"}`)
	if got := string(f.Response("s1", escaped)); !strings.Contains(got, "Acme") {
		t.Fatalf("escaped placeholder not restored: %s", got)
	}
	var check map[string]string
	if err := json.Unmarshal([]byte(restored), &check); err != nil {
		t.Fatalf("restored body is not valid JSON: %v", err)
	}
}

func TestFilterOffByDefaultAndUnknownSessionUntouched(t *testing.T) {
	f := &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, "Acme\tORG\n")}
	body := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"Acme"}]`)}
	if err := f.Request("s1", body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body["messages"]), "Acme") {
		t.Fatal("filter is off but the text was changed")
	}
	if got := string(f.Response("unknown", []byte("<ORG_1>"))); got != "<ORG_1>" {
		t.Fatalf("unknown session was changed: %s", got)
	}
}

func TestParseFilterTermsReportsBadLines(t *testing.T) {
	_, errs := parseFilterTerms("good\tORG\nno tab here\n\n# comment\n\tORG\n")
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}

func TestFilterRestoresStreamedAnswersEvenWhenAPlaceholderIsSplit(t *testing.T) {
	deltas := []string{"Ask <PER", "SON_1> about <HO", "ST_1", ">, a < b, and <PERSON_9> stays", " <ORG"}
	a := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range deltas {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", d)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	r, srv := newTestRouter(t, a)
	tf := &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, "John Smith\tPERSON\ndb.example.lan\tHOST\n"),
		cfg: filterConfig{Enabled: true, Terms: "x"}}
	r.filter, r.pipeline = tf, []Transform{tf}
	_, out := post(t, srv, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"John Smith on db.example.lan"}]}`, "X-Session-Id", "s1")
	var got strings.Builder
	for _, l := range strings.Split(out, "\n") {
		var ev struct {
			Choices []struct{ Delta struct{ Content string } } `json:"choices"`
		}
		if d, ok := strings.CutPrefix(l, "data: "); ok && json.Unmarshal([]byte(d), &ev) == nil && len(ev.Choices) > 0 {
			got.WriteString(ev.Choices[0].Delta.Content)
		}
	}
	if want := "Ask John Smith about db.example.lan, a < b, and <PERSON_9> stays <ORG"; got.String() != want {
		t.Fatalf("streamed answer:\n got %q\nwant %q", got.String(), want)
	}
}

func TestFilterAskShowsTheRoundTrip(t *testing.T) {
	// the model sees only placeholders and answers with them
	var got string
	a := newFake(t, func(w http.ResponseWriter, req *http.Request) {
		var b struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&b)
		got = b.Messages[0].Content
		okJSON("Sure, I will write to <PERSON_1> today.")(w, req)
	})
	r, _ := newTestRouter(t, a)
	r.filter = &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, "John Smith\tPERSON\n")}
	w := httptest.NewRecorder()
	r.handleFilterAsk(w, httptest.NewRequest("POST", "/api/filter/ask", strings.NewReader(`{"text":"Write to John Smith","model":"auto"}`)))
	var res struct{ Model, Masked, Answer, Restored, Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if got != "Write to <PERSON_1>" || res.Masked != got {
		t.Fatalf("the model got %q, shown %q", got, res.Masked)
	}
	if res.Answer != "Sure, I will write to <PERSON_1> today." || res.Restored != "Sure, I will write to John Smith today." || res.Model == "" {
		t.Fatalf("round trip: %+v", res)
	}
}

func TestFilterTryShowsWhatTheModelGets(t *testing.T) {
	a := newFake(t, okJSON("a"))
	r, _ := newTestRouter(t, a)
	r.filter = &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, "John Smith\tPERSON\ndb.example.lan\tHOST\n")}
	w := httptest.NewRecorder()
	r.handleFilterTry(w, httptest.NewRequest("POST", "/api/filter/try", strings.NewReader(`{"text":"Ask john smith about db.example.lan, then John Smith again"}`)))
	var got struct{ Masked string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if got.Masked != "Ask <PERSON_1> about <HOST_1>, then <PERSON_1> again" {
		t.Fatalf("masked: %q", got.Masked)
	}
}
