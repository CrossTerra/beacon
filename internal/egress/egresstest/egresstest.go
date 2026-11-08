// Package egresstest is the egress check for tests: a transport that records every host Beacon tries to contact and
// fails the test on any host not on the list it was given.
package egresstest

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/CrossTerra/beacon/internal/egress"
)

// Recorder is an http.RoundTripper.
type Recorder struct {
	t       testing.TB
	next    http.RoundTripper
	mu      sync.Mutex
	allowed map[string]bool
	seen    map[string]int
}

// New returns a recorder that allows only allowed and passes allowed requests to next.
func New(t testing.TB, allowed []string, next http.RoundTripper) *Recorder {
	r := &Recorder{t: t, next: next, allowed: map[string]bool{}, seen: map[string]int{}}
	for _, a := range allowed {
		r.allowed[strings.ToLower(a)] = true
	}
	return r
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	return wrapped{r, r.next}.RoundTrip(req)
}

// Wrap returns a transport that records and checks like r, sharing its record, and passes allowed requests to next.
func (r *Recorder) Wrap(next http.RoundTripper) http.RoundTripper { return wrapped{r, next} }

type wrapped struct {
	r    *Recorder
	next http.RoundTripper
}

func (w wrapped) RoundTrip(req *http.Request) (*http.Response, error) {
	a := egress.Address(req.URL)
	w.r.mu.Lock()
	w.r.seen[a]++
	ok := w.r.allowed[a]
	w.r.mu.Unlock()
	if !ok {
		w.r.t.Errorf("egress: Beacon tried to contact %s, which is not on the egress list", a)
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &egress.DeniedError{Address: a}
	}
	return w.next.RoundTrip(req)
}

// Seen returns every address a request was made to, sorted.
func (r *Recorder) Seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.seen))
	for a := range r.seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Count returns how many requests were made in all.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.seen {
		n += c
	}
	return n
}
