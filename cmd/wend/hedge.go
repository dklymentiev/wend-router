package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

// Hedged requests. Free tiers are often slow for a while and then fast
// again; waiting out a slow one costs the caller seconds it would not spend
// on the next model. With a profile's hedge_after_ms, a model that has sent
// no first token by then gets company: the same request to the next model.
// The first to stream answers the caller, the other is cancelled. Nothing
// reaches the caller before a first token, so the race is invisible to it.

// lostAttempt is a model that failed during a race.
type lostAttempt struct {
	m  *Model
	ae *attemptErr
}

// hedgeAfter is the profile's hedge delay for this request, 0 for none.
func (r *Router) hedgeAfter(ri *reqInfo) time.Duration {
	p := r.profile(ri.model)
	if p == nil || p.HedgeAfterMs <= 0 {
		return 0
	}
	return time.Duration(p.HedgeAfterMs) * time.Millisecond
}

// pickBackup is the model to race against m: the next one not tried yet,
// at another provider when there is one, since copies at the same provider
// tend to be slow together.
func pickBackup(m *Model, rest []*Model, tried []string) *Model {
	var same *Model
	for _, c := range rest {
		if containsKey(tried, c.Key) {
			continue
		}
		if c.Provider != m.Provider {
			return c
		}
		if same == nil {
			same = c
		}
	}
	return same
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// attempt asks m and, when backup is set, races it against backup after the
// hedge delay. It returns the model that served (or failed last), its error,
// the models tried so far, and the other failures to book.
func (r *Router) attempt(w http.ResponseWriter, req *http.Request, ri *reqInfo, m, backup *Model, after time.Duration, tried []string) (*Model, *attemptErr, []string, []lostAttempt) {
	deadline := r.cfg.RequestTimeout
	if ri.stream {
		deadline = 0
	}
	serve := func(x *Model, u *upstream) *attemptErr {
		if ri.stream {
			return r.serveStream(w, req, ri, x, u, tried)
		}
		return r.serveOnce(w, req, ri, x, u, tried)
	}
	if backup == nil {
		m.begin()
		defer m.end()
		u, ae := r.openStream(req, ri, m, deadline)
		if ae != nil {
			return m, ae, tried, nil
		}
		return m, serve(m, u), tried, nil
	}

	type opened struct {
		m  *Model
		u  *upstream
		ae *attemptErr
	}
	results := make(chan opened, 2)
	cancels := map[*Model]context.CancelFunc{}
	launch := func(x *Model) {
		ctx, cancel := context.WithCancel(req.Context())
		cancels[x] = cancel
		x.begin()
		go func() {
			u, ae := r.openStream(req.WithContext(ctx), ri, x, deadline)
			results <- opened{x, u, ae}
		}()
	}
	// discard drains a cancelled racer in the background.
	discard := func() {
		go func() {
			o := <-results
			if o.u != nil {
				o.u.close()
			}
			o.m.end()
		}()
	}

	launch(m)
	timer := time.NewTimer(after)
	defer timer.Stop()
	running, raced := 1, false
	var lost []lostAttempt
	for running > 0 {
		select {
		case <-timer.C:
			if !raced && req.Context().Err() == nil {
				raced = true
				tried = append(tried, backup.Key)
				log.Printf("%s: no first token after %s, asking %s too", m.Key, after, backup.Key)
				launch(backup)
				running++
			}
		case o := <-results:
			running--
			if o.ae == nil {
				for x, cancel := range cancels {
					if x != o.m {
						cancel()
					}
				}
				if running > 0 {
					discard()
				}
				if o.m != m {
					m.noteSlow(after) // m lost the race: slower than it should be
				}
				defer o.m.end()
				return o.m, serve(o.m, o.u), tried, lost
			}
			o.m.end()
			if req.Context().Err() != nil {
				for _, cancel := range cancels {
					cancel()
				}
				if running > 0 {
					discard()
				}
				return o.m, &attemptErr{clientGone: true}, tried, lost
			}
			if !raced {
				// m failed before the hedge delay: an ordinary failure, the
				// backup comes next in the usual order.
				for _, cancel := range cancels {
					cancel()
				}
				return o.m, o.ae, tried, nil
			}
			lost = append(lost, lostAttempt{o.m, o.ae})
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	final := lost[len(lost)-1]
	return final.m, final.ae, tried, lost[:len(lost)-1]
}
