// Package signal is Beacon's client for Signal's HTTP API. It writes events with POST /api/events, reads the
// restricted subject prefixes with GET /api/restrictions, and checks a key with GET /api/me. It reads no log or thread.
package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
)

// Outcome is how Signal answered a write, once retries are done.
type Outcome string

const (
	// Written is 201: a new event.
	Written Outcome = "written"
	// Replayed is 200 with replayed: the same key and the same bytes were already written.
	Replayed Outcome = "replayed"
	// Conflict is 409 idempotency_conflict: the key holds a different request. It is final for that key.
	Conflict Outcome = "conflict"
	// Refused is any other final answer for this body: Signal will refuse the same bytes again.
	Refused Outcome = "refused"
)

// Result is Signal's answer to one write.
type Result struct {
	Outcome Outcome
	Status  int
	EventID string
	// Code and Message are Signal's error code and message, when it refused.
	Code    string
	Message string
}

// ErrUnauthorized means the key was refused (401). Nothing will succeed until the key is fixed, so sending stops.
var ErrUnauthorized = errors.New("signal: the key was refused (401)")

// ErrUnavailable means the write did not get a final answer within the attempts allowed. The body stays pending.
var ErrUnavailable = errors.New("signal: no final answer; the body stays pending")

// ErrNoRead means the key may not read the restricted prefixes (403): it has no read scope.
var ErrNoRead = errors.New("signal: the key cannot read the restricted prefixes (403); it needs the read scope")

// Client writes events.
type Client struct {
	base   *url.URL
	http   *http.Client
	secret keychain.Secret

	// Attempts is how many times one call tries before it gives up with ErrUnavailable.
	Attempts int
	// Backoff is the wait before the second attempt after a 5xx or network error; it doubles up to MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration
	// MaxRetryAfter caps how long a 429 makes the client wait.
	MaxRetryAfter time.Duration
	// Sleep waits, or returns the context's error. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// New returns a client for the Signal at base. rt is the transport, which the caller wraps in the egress guard.
func New(base *url.URL, rt http.RoundTripper, secret keychain.Secret) *Client {
	return &Client{
		base:   base,
		secret: secret,
		http: &http.Client{
			Transport: rt,
			Timeout:   30 * time.Second,
			// A write is never followed elsewhere: a redirect is an answer, not a new destination.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Attempts:      5,
		Backoff:       time.Second,
		MaxBackoff:    30 * time.Second,
		MaxRetryAfter: 2 * time.Minute,
		Sleep:         sleep,
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Write sends body to POST /api/events, retrying 429 after Retry-After and 5xx or network errors with backoff.
// The bytes are sent exactly as given on every attempt.
func (c *Client) Write(ctx context.Context, body []byte) (Result, error) {
	backoff := c.Backoff
	var last error
	for attempt := 1; attempt <= c.Attempts; attempt++ {
		res, wait, err := c.once(ctx, body)
		if err == nil {
			return res, nil
		}
		var again retryable
		if !errors.As(err, &again) || ctx.Err() != nil {
			return Result{}, err
		}
		last = err
		if attempt == c.Attempts {
			break
		}
		if wait == 0 {
			wait = backoff
			backoff = min(backoff*2, c.MaxBackoff)
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return Result{}, err
		}
	}
	return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, last)
}

type retryable struct {
	status int
	code   string
}

func (r retryable) Error() string {
	if r.status == 0 {
		return "network error"
	}
	return fmt.Sprintf("status %d %s", r.status, r.code)
}

// once makes one attempt. A nil error is a final answer. A non-nil error may come with how long to wait.
func (c *Client) once(ctx context.Context, body []byte) (Result, time.Duration, error) {
	u := c.base.JoinPath("/api/events")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.secret.Authorize(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, 0, ctx.Err()
		}
		var denied *egress.DeniedError
		if errors.As(err, &denied) {
			return Result{}, 0, err
		}
		return Result{}, 0, retryable{}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
		Replayed bool `json:"replayed"`
		Error    struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &parsed)
	res := Result{Status: resp.StatusCode, EventID: parsed.Event.ID, Code: parsed.Error.Code, Message: parsed.Error.Message}

	switch {
	case resp.StatusCode == http.StatusCreated && res.EventID != "":
		res.Outcome = Written
		return res, 0, nil
	case resp.StatusCode == http.StatusOK && parsed.Replayed && res.EventID != "":
		res.Outcome = Replayed
		return res, 0, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return Result{}, 0, ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return Result{}, c.retryAfter(resp.Header.Get("Retry-After")), retryable{status: resp.StatusCode, code: res.Code}
	case resp.StatusCode >= 500:
		return Result{}, 0, retryable{status: resp.StatusCode, code: res.Code}
	case resp.StatusCode == http.StatusConflict && res.Code == "idempotency_conflict":
		res.Outcome = Conflict
		return res, 0, nil
	case resp.StatusCode == http.StatusConflict && res.Code == "conflict" && identityMoved(data):
		// The identity named in `by` moved while Signal wrote; Signal asks for the same request again.
		return Result{}, 0, retryable{status: resp.StatusCode, code: res.Code}
	default:
		res.Outcome = Refused
		if res.Code == "" {
			res.Code = "http_" + strconv.Itoa(resp.StatusCode)
		}
		return res, 0, nil
	}
}

func identityMoved(data []byte) bool {
	var p struct {
		Error struct {
			Details struct {
				Field string `json:"field"`
			} `json:"details"`
		} `json:"error"`
	}
	return json.Unmarshal(data, &p) == nil && p.Error.Details.Field == "/by/identity"
}

func (c *Client) retryAfter(h string) time.Duration {
	d := time.Second
	if s, err := strconv.Atoi(h); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = time.Until(t)
	}
	return max(time.Second, min(d, c.MaxRetryAfter))
}

// Me is who a key is, from GET /api/me. It holds no secret.
type Me struct {
	// ID is the actor id (act_…). Handle is the handle without "@", when it has one.
	ID       string
	Type     string
	Handle   string
	WorksFor string
	Scopes   []string
}

// Me checks the key with GET /api/me. A 401 is ErrUnauthorized. Any other failure is ErrUnavailable. It makes one
// attempt: beacon key set stores the key only when this succeeds.
func (c *Client) Me(ctx context.Context) (Me, error) {
	u := c.base.JoinPath("/api/me")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Me{}, err
	}
	req.Header.Set("Accept", "application/json")
	c.secret.Authorize(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Me{}, ctx.Err()
		}
		var denied *egress.DeniedError
		if errors.As(err, &denied) {
			return Me{}, err
		}
		return Me{}, ErrUnavailable
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return Me{}, ErrUnauthorized
	case http.StatusOK:
	default:
		return Me{}, ErrUnavailable
	}
	var parsed struct {
		Actor struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Handle   string `json:"handle"`
			WorksFor string `json:"works_for"`
		} `json:"actor"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Actor.ID == "" {
		return Me{}, ErrUnavailable
	}
	return Me{ID: parsed.Actor.ID, Type: parsed.Actor.Type, Handle: parsed.Actor.Handle, WorksFor: parsed.Actor.WorksFor, Scopes: parsed.Scopes}, nil
}

// Restrictions returns the restricted subject prefixes (`gmail:*`), from GET /api/restrictions. It makes one attempt:
// a failure is ErrUnauthorized, ErrNoRead or ErrUnavailable, and the caller sends nothing personal until a later
// pass reads them.
func (c *Client) Restrictions(ctx context.Context) ([]string, error) {
	u := c.base.JoinPath("/api/restrictions")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.secret.Authorize(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var denied *egress.DeniedError
		if errors.As(err, &denied) {
			return nil, err
		}
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return nil, ErrNoRead
	case resp.StatusCode != http.StatusOK:
		return nil, ErrUnavailable
	}
	var parsed struct {
		Data []struct {
			Prefix string `json:"prefix"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	// A list Beacon cannot read whole is not proof of anything: fail closed.
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Data == nil || (parsed.NextCursor != nil && *parsed.NextCursor != "") {
		return nil, ErrUnavailable
	}
	out := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		out = append(out, d.Prefix)
	}
	return out, nil
}
