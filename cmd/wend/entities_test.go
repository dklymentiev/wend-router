package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func entityFilter(t *testing.T, terms string) *termFilter {
	t.Helper()
	cat, err := parseEntities(defaultEntities)
	if err != nil {
		t.Fatal(err)
	}
	return &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, terms), cat: cat,
		cfg: filterConfig{Enabled: true, Terms: terms}}
}

func mask(f *termFilter, text string) string {
	sess := &filterSession{restore: map[string]string{}, byValue: map[string]string{}, counters: map[string]int{}}
	return f.maskText(sess, text, f.terms)
}

// Every built-in entity catches what it is for and leaves alone what only looks like it.
func TestEntitiesCatchTheirKindAndLeaveLookalikes(t *testing.T) {
	cases := []struct{ id, in, want string }{
		{"email", "write to jane.doe@example.net today", "write to <EMAIL_1> today"},
		{"email", "see a@example.com", "see a@example.com"},
		{"card", "card 4111 1111 1111 1111, exp soon", "card <CARD_1>, exp soon"},
		{"card", "card 4111-1111-1111-1111", "card <CARD_1>"},
		{"card", "order 4111 1111 1111 1112", "order 4111 1111 1111 1112"}, // fails the check digit
		{"iban", "pay to GB82 WEST 1234 5698 7654 32 please", "pay to <IBAN_1> please"},
		{"iban", "pay to GB00 WEST 1234 5698 7654 32 please", "pay to GB00 WEST 1234 5698 7654 32 please"},
		{"account", "account number: 123456789", "account number: <ACCOUNT_1>"},
		{"account", "номер счета 40817810099910004312", "номер счета <ACCOUNT_1>"},
		{"ssn", "SSN 123-45-6789", "SSN <SSN_1>"},
		{"ssn", "SSN 000-45-6789", "SSN 000-45-6789"}, // never issued
		{"phone", "call (312) 555-0100", "call <PHONE_1>"},
		{"phone", "call +44 20 7946 0958", "call <PHONE_1>"},
		{"phone", "timestamp 1794248569", "timestamp 1794248569"},
		{"passport", "passport: X1234567", "passport: <PASSPORT_1>"},
		{"dob", "DOB: 1990-04-12", "DOB: <DOB_1>"},
		{"dob", "дата рождения: 12.04.1990", "дата рождения: <DOB_1>"},
		{"apikey", "key sk-ant-" + strings.Repeat("a", 30), "key <SECRET_1>"},
		{"apikey", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123", "Authorization: Bearer <SECRET_1>"},
		{"apikey", "token = buildSessionToken()", "token = buildSessionToken()"},
		{"password", "password: hunter2xyz", "password: <PASSWORD_1>"},
		{"password", "пароль: qwerty123", "пароль: <PASSWORD_1>"},
		{"password", "postgres://app:s3cret@db:5432/x", "postgres://app:<PASSWORD_1>@db:5432/x"},
		{"ip", "ssh 192.0.2.44", "ssh <IP_1>"},
		{"ip", "on 127.0.0.1 and version 1.2.300.4", "on 127.0.0.1 and version 1.2.300.4"},
		{"url", "see https://wiki.acme.com/page", "see <URL_1>"},
		{"url", "see https://example.com/page", "see https://example.com/page"},
	}
	for _, c := range cases {
		f := entityFilter(t, "@"+c.id+"\tX\n")
		if got := mask(f, c.in); got != c.want {
			t.Errorf("%s: %q\n got %q\nwant %q", c.id, c.in, got, c.want)
		}
	}
}

// Entities and listed terms work together, and the answer gets the values back.
func TestEntitiesRoundTripWithTerms(t *testing.T) {
	f := entityFilter(t, "Acme\tORG\n@card\tCARD\n@email\tEMAIL\n")
	body := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"Acme card 4111 1111 1111 1111, mail jane@example.net"}]`)}
	if err := f.Request("s1", body); err != nil {
		t.Fatal(err)
	}
	if got := string(body["messages"]); !strings.Contains(got, "<ORG_1> card <CARD_1>, mail <EMAIL_1>") {
		t.Fatalf("masked: %s", got)
	}
	back := string(f.Response("s1", []byte(`{"content":"Charged <CARD_1> for <ORG_1>, receipt to <EMAIL_1>"}`)))
	if !strings.Contains(back, "Charged 4111 1111 1111 1111 for Acme, receipt to jane@example.net") {
		t.Fatalf("restored: %s", back)
	}
}

// A list that names an entity the catalogue does not have is refused: saving it
// fails, and a request is stopped rather than sent unmasked.
func TestUnknownEntityIsRefused(t *testing.T) {
	f := entityFilter(t, "@nosuch\tX\n")
	body := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`)}
	if err := f.Request("s1", body); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("request with an unknown entity: %v", err)
	}
	a := newFake(t, okJSON("a"))
	r, _ := newTestRouter(t, a)
	r.filter = entityFilter(t, "")
	w := httptest.NewRecorder()
	r.handleFilter(w, httptest.NewRequest("PUT", "/api/filter", strings.NewReader(`{"enabled":true,"terms":"@nosuch\tX\n"}`)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "nosuch") {
		t.Fatalf("saving an unknown entity: %d %s", w.Code, w.Body.String())
	}
}

// The catalogue is a file: written from the built-in one on first start, then
// read as the user left it, own entities included.
func TestEntityCatalogueIsAnEditableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entities.json")
	cat, err := loadEntities(path)
	if err != nil || cat.byID["card"] == nil {
		t.Fatalf("first start: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), `"id": "card"`) {
		t.Fatalf("the built-in catalogue was not written: %v", err)
	}
	own := `{"entities":[{"id":"contract","name":"Any contract number","type":"CONTRACT","patterns":[{"re":"\\bACM-\\d{6}\\b"}]}]}`
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err = loadEntities(path)
	if err != nil || cat.byID["contract"] == nil || cat.byID["card"] != nil {
		t.Fatalf("own catalogue: %v %+v", err, cat)
	}
	f := &termFilter{sessions: map[string]*filterSession{}, terms: testTerms(t, "@contract\tCONTRACT\n"), cat: cat}
	if got := mask(f, "see ACM-123456 and ACM-12"); got != "see <CONTRACT_1> and ACM-12" {
		t.Fatalf("own entity: %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"entities":[{"id":"x","type":"X","patterns":[{"re":"(unclosed"}]}]}`), 0o644); err == nil {
		if _, err := loadEntities(path); err == nil {
			t.Fatal("a broken pattern was accepted")
		}
	}
}
