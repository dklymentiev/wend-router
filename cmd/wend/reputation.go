package main

import (
	"math"
	"sync"
	"time"
)

// Reputation is how a model or provider usually behaves: the share of good
// outcomes, where older outcomes count for less (half the weight every
// three days). It needs three numbers and no history, and it forgives a
// model that got better.

const (
	repHalfLife = 72 * time.Hour
	repMinSeen  = 5 // outcomes before a label other than "new"
)

// repData is what is saved: three numbers.
type repData struct {
	Good  float64   `json:"good"`
	Total float64   `json:"total"`
	At    time.Time `json:"at"`
}

type reputation struct {
	mu sync.Mutex
	repData
}

func (r *reputation) add(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.At.IsZero() {
		f := math.Pow(0.5, float64(now.Sub(r.At))/float64(repHalfLife))
		r.Good, r.Total = r.Good*f, r.Total*f
	}
	if ok {
		r.Good++
	}
	r.Total++
	r.At = now
}

// share is the good share and how much evidence stands behind it.
func (r *reputation) share() (float64, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Total == 0 {
		return 0, 0
	}
	return r.Good / r.Total, r.Total
}

func (r *reputation) label() string {
	s, n := r.share()
	switch {
	case n < repMinSeen:
		return "new"
	case s >= 0.95:
		return "stable"
	case s >= 0.8:
		return "mostly ok"
	}
	return "flaky"
}

// flaky is a known unreliable one; a new one is not.
func (r *reputation) flaky() bool {
	return r.label() == "flaky"
}

func (r *reputation) snapshot() repData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repData
}

func (r *reputation) restore(d repData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repData = d
}

// countsForReputation says whether an outcome says something about how
// reliable a model is. Refusals by design (paid-only, not found), a full
// provider (busy) and bad requests do not.
func countsForReputation(k failKind) bool {
	switch k {
	case failTransient, failEmpty, failQuota:
		return true
	}
	return false
}
