// Package limited performs HTTP requests for sources and honours rate-limit headers. A wait longer than MaxWait
// ends the pass instead of sleeping, so the caller resumes from the committed cursor.
package limited

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Client wraps an HTTP client with rate-limit backoff.
type Client struct {
	HTTP *http.Client
	// MaxWait is the longest a pass waits. Zero means two minutes, the same cap as Signal's Retry-After.
	MaxWait time.Duration
	// Sleep waits, or returns the context's error. Nil sleeps on a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time

	waitUntil time.Time
}

// HTTPClient is the client a source uses: no redirects, a bounded timeout. The caller supplies the transport,
// which is the egress guard.
func HTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: rt,
		Timeout:   30 * time.Second,
		// A redirect is an answer, not a new destination: the next host would not be one this request chose.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *Client) maxWait() time.Duration {
	if c.MaxWait > 0 {
		return c.MaxWait
	}
	return 2 * time.Minute
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do sends req, retrying a rate-limit response whose wait is within MaxWait. The request body is buffered so a
// retry sends the same bytes. A longer wait returns source.ErrRateLimited without sleeping it.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		r, err := clone(req, body)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(r.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		wait, limited := rateWait(resp, c.now())
		if !limited {
			c.note(resp)
			return resp, nil
		}
		resp.Body.Close()
		if wait > c.maxWait() {
			return nil, source.ErrRateLimited
		}
		if wait < time.Second {
			wait = time.Second
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, source.ErrRateLimited
}

func (c *Client) wait(ctx context.Context) error {
	if c.waitUntil.IsZero() {
		return nil
	}
	d := c.waitUntil.Sub(c.now())
	if d <= 0 {
		return nil
	}
	if d > c.maxWait() {
		return source.ErrRateLimited
	}
	return c.sleep(ctx, d)
}

func (c *Client) note(resp *http.Response) {
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	if reset := resetTime(resp.Header.Get("X-RateLimit-Reset")); !reset.IsZero() {
		c.waitUntil = reset
	}
}

func clone(req *http.Request, body []byte) (*http.Request, error) {
	r := req.Clone(req.Context())
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	return r, nil
}

// rateWait reports how long a rate-limit response asks us to wait. A 403 is a rate limit only when it carries
// Retry-After or a spent X-RateLimit-Remaining; other 403s are the caller's to handle.
func rateWait(resp *http.Response, now time.Time) (time.Duration, bool) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
	case http.StatusForbidden:
		if resp.Header.Get("Retry-After") == "" && resp.Header.Get("X-RateLimit-Remaining") != "0" {
			return 0, false
		}
	default:
		return 0, false
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
		if when, err := http.ParseTime(ra); err == nil {
			return when.Sub(now), true
		}
	}
	if reset := resetTime(resp.Header.Get("X-RateLimit-Reset")); !reset.IsZero() {
		return reset.Sub(now), true
	}
	return time.Second, true
}

func resetTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
