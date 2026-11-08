// Package egress lists every host Beacon may contact under a config, and guards the transport so it contacts no
// other (DEC-14). `beacon egress` prints the list; every HTTP request Beacon makes goes through Guard.
package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Host is one host Beacon may contact, and why.
type Host struct {
	// Address is host:port.
	Address string
	// Purpose says what Beacon sends there.
	Purpose string
}

// DeniedError is returned for a request to a host not on the list.
type DeniedError struct {
	Address string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("egress: %s is not on the egress list; Beacon did not contact it", e.Address)
}

// Address returns u's host:port, lowercased, with the scheme's default port filled in.
func Address(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// List returns every host Beacon may contact: the Signal host, and each enabled source's hosts. A hosted classifier
// is added here when one exists (DEC-7); Phase 1 has none.
func List(signal *url.URL, sources []source.Source) []Host {
	hosts := []Host{{Address: Address(signal), Purpose: "signal: events Beacon sends, and lease claims"}}
	for _, s := range sources {
		for _, h := range s.Hosts() {
			hosts = append(hosts, Host{Address: h, Purpose: "source " + s.Name()})
		}
	}
	return Sorted(hosts)
}

// Sorted returns hosts sorted by address, with duplicates merged.
func Sorted(hosts []Host) []Host {
	byAddr := map[string][]string{}
	for _, h := range hosts {
		byAddr[h.Address] = append(byAddr[h.Address], h.Purpose)
	}
	out := make([]Host, 0, len(byAddr))
	for a, ps := range byAddr {
		sort.Strings(ps)
		out = append(out, Host{Address: a, Purpose: strings.Join(dedupe(ps), "; ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Addresses returns the addresses of hosts.
func Addresses(hosts []Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Address)
	}
	return out
}

type guard struct {
	allowed map[string]bool
	next    http.RoundTripper
}

// Guard wraps next so that a request to any address not in allowed fails without a connection.
func Guard(allowed []string, next http.RoundTripper) http.RoundTripper {
	g := &guard{allowed: map[string]bool{}, next: next}
	for _, a := range allowed {
		g.allowed[strings.ToLower(a)] = true
	}
	return g
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	a := Address(req.URL)
	if !g.allowed[a] {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, &DeniedError{Address: a}
	}
	return g.next.RoundTrip(req)
}

// Transport is the transport Beacon uses: no proxy from the environment (a proxy would be a host not on the list),
// TLS verified, and guarded by allowed.
func Transport(allowed []string) http.RoundTripper {
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return Guard(allowed, t)
}
