package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Entities are kinds of values the filter can mask without being told each value:
// any email, any card number, any password after "password:". The catalogue is a
// file the user can edit (entities.json next to the binary, or ROUTER_ENTITIES);
// the router writes the built-in one there on first start and then reads only the
// file. A filter list names an entity with a line "@<id><TAB>TYPE".

//go:embed entities.json
var defaultEntities []byte

type entityPattern struct {
	Re    string `json:"re"`
	Group int    `json:"group,omitempty"` // mask only this capture group (the value after a keyword)
	re    *regexp.Regexp
}

type entity struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Patterns []entityPattern `json:"patterns"`
	Check    string          `json:"check,omitempty"`  // luhn, iban, ssn or ip: a match must also pass this
	Except   []string        `json:"except,omitempty"` // a match containing one of these is left alone
}

type entityCatalog struct {
	Entities []*entity `json:"entities"`
	byID     map[string]*entity
}

func entitiesPath() string {
	if p := os.Getenv("ROUTER_ENTITIES"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "entities.json")
	}
	return "entities.json"
}

// loadEntities reads the catalogue, writing the built-in one first when the file
// is missing. A file that does not parse is an error, not a reason to fall back:
// the filter then refuses requests that name an entity (see termFilter.Request).
func loadEntities(path string) (*entityCatalog, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		raw = defaultEntities
		if werr := os.WriteFile(path, raw, 0o644); werr != nil {
			log.Printf("filter: could not write the entity catalogue to %s: %v (using the built-in one)", path, werr)
		}
	} else if err != nil {
		return nil, err
	}
	return parseEntities(raw)
}

func parseEntities(raw []byte) (*entityCatalog, error) {
	var c entityCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("entities: %w", err)
	}
	c.byID = map[string]*entity{}
	for _, e := range c.Entities {
		if e.ID == "" || e.Type == "" || len(e.Patterns) == 0 {
			return nil, fmt.Errorf("entities: %q needs an id, a type and a pattern", e.ID)
		}
		switch e.Check {
		case "", "luhn", "iban", "ssn", "ip":
		default:
			return nil, fmt.Errorf("entities: %s: unknown check %q", e.ID, e.Check)
		}
		for i := range e.Patterns {
			re, err := regexp.Compile(e.Patterns[i].Re)
			if err != nil {
				return nil, fmt.Errorf("entities: %s: %w", e.ID, err)
			}
			if e.Patterns[i].Group > re.NumSubexp() {
				return nil, fmt.Errorf("entities: %s: group %d, the pattern has %d", e.ID, e.Patterns[i].Group, re.NumSubexp())
			}
			e.Patterns[i].re = re
		}
		e.Type = strings.ToUpper(e.Type)
		c.byID[e.ID] = e
	}
	return &c, nil
}

// entitySpans finds the values of the given entities in text, as rune offsets.
func entitySpans(text string, ents []*entity) []filterSpan {
	if len(ents) == 0 {
		return nil
	}
	// byte offset -> rune offset; matches start and end on rune starts only
	runeAt := make([]int, len(text)+1)
	n := 0
	for i := range text {
		runeAt[i] = n
		n++
	}
	runeAt[len(text)] = n
	var spans []filterSpan
	for _, e := range ents {
		for _, p := range e.Patterns {
			for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
				s, t := m[2*p.Group], m[2*p.Group+1]
				if s < 0 || s == t {
					continue
				}
				value := text[s:t]
				if !entityCheck(e.Check, value) || excepted(value, e.Except) {
					continue
				}
				spans = append(spans, filterSpan{start: runeAt[s], end: runeAt[t], typ: e.Type})
			}
		}
	}
	return spans
}

func excepted(value string, except []string) bool {
	v := strings.ToLower(value)
	for _, x := range except {
		if x != "" && strings.Contains(v, strings.ToLower(x)) {
			return true
		}
	}
	return false
}

func entityCheck(check, value string) bool {
	switch check {
	case "luhn":
		return luhn(digitsOf(value))
	case "iban":
		return ibanValid(value)
	case "ssn":
		return ssnValid(digitsOf(value))
	case "ip":
		return ipValid(value)
	}
	return true
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhn is the check digit of card numbers: a random number of the right length
// passes it one time in ten, so most long numbers that are not cards stay as they are.
func luhn(d string) bool {
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum := 0
	for i := 0; i < len(d); i++ {
		x := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			x *= 2
			if x > 9 {
				x -= 9
			}
		}
		sum += x
	}
	return sum%10 == 0
}

// ibanValid: the country letters and check digits moved to the end, letters as
// numbers, the whole thing modulo 97 must be 1.
func ibanValid(s string) bool {
	var c strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r != ' ' {
			c.WriteRune(r)
		}
	}
	v := c.String()
	if len(v) < 15 || len(v) > 34 {
		return false
	}
	v = v[4:] + v[:4]
	var num strings.Builder
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			num.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			num.WriteString(fmt.Sprint(int(r-'A') + 10))
		default:
			return false
		}
	}
	n, ok := new(big.Int).SetString(num.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// ssnValid leaves out numbers that are never issued: area 000, 666 or 900-999,
// group 00, serial 0000.
func ssnValid(d string) bool {
	if len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

func ipValid(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
			return false
		}
		n := 0
		for _, r := range p {
			if !unicode.IsDigit(r) {
				return false
			}
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}
