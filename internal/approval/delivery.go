package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
)

// RecordTransport sends only an already-persisted Signal request. A missing/uncertain answer must be
// returned as a retry with Ambiguous set.
type RecordTransport interface {
	PostRecord(context.Context, []byte) RecordAnswer
}

type RecordAnswer struct {
	EventID    string
	Stopped    string
	RetryAfter time.Duration
	Ambiguous  bool
}

type DrainResult struct {
	Sent       int
	Stopped    string
	Retry      bool
	RetryAfter time.Duration
}

// Drain delivers the head, and only advances after a durable acknowledgement. All calls on one journal
// are serialized; failure at any record holds up every record after it.
func (j *Journal) Drain(ctx context.Context, who SignalIdentity, transport RecordTransport) (DrainResult, error) {
	j.draining.Lock()
	defer j.draining.Unlock()
	var out DrainResult
	for ctx.Err() == nil {
		b, err := j.beginDelivery(who)
		if err != nil {
			return out, err
		}
		if b == nil {
			out.Stopped = j.DeliveryStatus().Stopped
			return out, nil
		}
		answer := transport.PostRecord(ctx, b)
		switch {
		case ValidID(answer.EventID, "evt"):
			if err := j.endDelivery("ack", answer.EventID, false); err != nil {
				return out, err
			}
			out.Sent++
		case answer.Stopped != "":
			out.Stopped = answer.Stopped
			return out, j.endDelivery("stop", answer.Stopped, answer.Ambiguous)
		default:
			out.Retry, out.RetryAfter = true, answer.RetryAfter
			return out, j.endDelivery("retry", "", answer.Ambiguous)
		}
	}
	return out, ctx.Err()
}

type SignalRecords struct {
	endpoint  string
	client    *http.Client
	authorize func(http.Header)
}

// RecordEgress is the single host used for record delivery, shared by the client and beacon egress.
// HTTP is accepted only on a literal loopback IP for fakes.
func RecordEgress(origin string) (egress.Host, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || (origin != u.Scheme+"://"+u.Host) || u.User != nil || strings.HasSuffix(u.Host, ":") ||
		(u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) {
		return egress.Host{}, errors.New("invalid Signal record origin")
	}
	return egress.Host{Address: egress.Address(u), Purpose: "Beacon Go service: approval records (POST /api/app-records)"}, nil
}

// NewSignalRecords takes an opaque authorization hook from the credential package, already bound to origin
// and the app actor. This package never reads or persists a key.
func NewSignalRecords(origin string, authorize func(http.Header)) (*SignalRecords, error) {
	host, err := RecordEgress(origin)
	if err != nil {
		return nil, err
	}
	if authorize == nil {
		return nil, errors.New("no Signal record authorization")
	}
	return &SignalRecords{endpoint: origin + "/api/app-records", authorize: authorize, client: &http.Client{
		Transport: egress.Transport([]string{host.Address}), Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (s *SignalRecords) PostRecord(ctx context.Context, b []byte) RecordAnswer {
	if len(b) == 0 || len(b) > maxRecord {
		return RecordAnswer{Stopped: "record:/body"}
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(b))
	if err != nil {
		return RecordAnswer{Ambiguous: true}
	}
	r.GetBody = nil // let the ordered outbox, not the transport, own replay
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "beacon-app")
	s.authorize(r.Header)
	res, err := s.client.Do(r)
	if err != nil {
		return RecordAnswer{Ambiguous: true}
	}
	defer res.Body.Close()
	b, err = io.ReadAll(io.LimitReader(res.Body, 64<<10+1))
	var answer recordReply
	if err != nil || len(b) > 64<<10 || json.Unmarshal(b, &answer) != nil {
		answer = recordReply{}
	}
	return classify(res.StatusCode, answer, res.Header.Get("Retry-After"), time.Now())
}

func (s *SignalRecords) Close() { s.client.CloseIdleConnections() }

type recordReply struct {
	EventID  string `json:"event_id"`
	Replayed *bool  `json:"replayed"`
	Error    struct {
		Code    string `json:"code"`
		Details struct {
			Reason string `json:"reason"`
			Field  string `json:"field"`
		} `json:"details"`
	} `json:"error"`
}

func classify(status int, reply recordReply, retry string, now time.Time) RecordAnswer {
	field := reply.Error.Details.Field
	switch {
	case status == 200 || status == 201:
		if ValidID(reply.EventID, "evt") && reply.Replayed != nil && *reply.Replayed == (status == 200) {
			return RecordAnswer{EventID: reply.EventID}
		}
		return RecordAnswer{Ambiguous: true}
	case status == 409:
		reason := reply.Error.Details.Reason
		if !slices.Contains([]string{"approval_binding", "expiry", "journal_order", "proposal_exists", "proposal_not_found", "runner_mismatch", "transition"}, reason) {
			if reply.Error.Code == "idempotency_conflict" {
				reason = "idempotency_conflict"
			} else {
				reason = "conflict"
			}
		}
		return RecordAnswer{Stopped: "conflict:" + reason}
	case status == 429:
		return RecordAnswer{RetryAfter: retryDelay(retry, now)}
	case status == 401 || status == 403 && field != "/subject" || status == 400 && field == "/occurred_at":
		return RecordAnswer{}
	case status >= 400 && status < 500:
		// Do not echo arbitrary field names, reason codes, or bodies from Signal or an interposed server.
		reason := "refused:" + strconv.Itoa(status)
		if slices.Contains([]string{"/subject", "/metadata/person", "/metadata/params_hash", "/metadata/approved_hash", "/metadata/approved_params_hash", "/key"}, field) {
			reason += ":" + field
		}
		return RecordAnswer{Stopped: reason}
	default:
		return RecordAnswer{RetryAfter: retryDelay(retry, now), Ambiguous: true}
	}
}

func retryDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if n, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(max(0, min(n, 3600))) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(0, min(when.Sub(now), time.Hour))
	}
	return 0
}
