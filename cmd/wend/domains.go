package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Domains say what a model is good for. Three sources, weakest first: the
// model's name and description, what the router knows about it (tools,
// vision, reasoning, context, speed), and tags set in the seed. The test
// mode will add measured ones.

var domainNames = []string{"code", "reasoning", "agents", "vision", "long", "fast"}

const (
	longContext = 256000
	fastTPS     = 150
)

var (
	codeName   = regexp.MustCompile(`(?i)(code|coder|codestral|devstral|laguna|starcoder|codegen)`)
	codeText   = regexp.MustCompile(`(?i)\b(coding|code generation|software engineering|programming|agentic coding)\b`)
	reasonName = regexp.MustCompile(`(?i)(reason|think|magistral|-r1\b|lean)`)
)

// guessDomains reads a model's id and description.
func guessDomains(id, description string) []string {
	var d []string
	if codeName.MatchString(id) || codeText.MatchString(description) {
		d = append(d, "code")
	}
	if reasonName.MatchString(id) {
		d = append(d, "reasoning")
	}
	return d
}

// domains is everything the router can say the model is good for now.
func (m *Model) domains() []string {
	m.mu.Lock()
	d := slices.Clone(m.tags)
	t := m.test
	if t != nil {
		// Measured beats guessed: what the name suggested gives way to
		// what the tests found.
		d = slices.DeleteFunc(d, func(x string) bool { return x == "code" || x == "reasoning" || x == "agents" })
		for _, dom := range []string{"code", "reasoning", "agents"} {
			if rate, n := t.rate(dom); n > 0 && rate >= 0.5 {
				d = append(d, dom)
			}
		}
	} else {
		if m.tools == capYes {
			d = append(d, "agents")
		}
		if m.reasoning == capYes {
			d = append(d, "reasoning")
		}
	}
	if m.vision == capYes {
		d = append(d, "vision")
	}
	if m.context >= longContext {
		d = append(d, "long")
	}
	if m.tps >= fastTPS {
		d = append(d, "fast")
	}
	m.mu.Unlock()
	slices.SortFunc(d, func(a, b string) int {
		return slices.Index(domainNames, a) - slices.Index(domainNames, b)
	})
	return slices.Compact(d)
}

func validDomains(ds []string) error {
	for _, d := range ds {
		if !slices.Contains(domainNames, d) {
			return fmt.Errorf("unknown domain %q (known: %s)", d, strings.Join(domainNames, ", "))
		}
	}
	return nil
}
