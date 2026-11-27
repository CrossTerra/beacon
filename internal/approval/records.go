package approval

import (
	"strconv"
	"strings"
	"time"
)

const maxRecord = 1 << 20

var (
	replayNonce = pattern(`[0-9a-f]{32}`)
	actorHandle = pattern(`[a-z0-9][a-z0-9._-]{0,63}`)
)

// SignalIdentity is supplied by trusted Signal settings, never the proposer.
type SignalIdentity struct {
	Person string
	Handle string
}

func (s SignalIdentity) Valid() bool {
	return ValidID(s.Person, "act") && ValidSignalHandle(s.Handle)
}

// ValidSignalHandle checks the handle without its leading @.
func ValidSignalHandle(handle string) bool { return actorHandle.matches(handle) }

type Result struct {
	ConfirmedBy string `json:"confirmed_by"`
	Native      string `json:"native,omitempty"`
	Error       string `json:"error,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
	Text        string `json:"result,omitempty"`
}

func (r Result) check(action Action) error {
	if r.ConfirmedBy != "reply" && r.ConfirmedBy != "read_back" && !(r.ConfirmedBy == "pre_dispatch" && r.Error != "") {
		return Invalid("/confirmed_by")
	}
	if r.Error != "" {
		if !statusCode.matches(r.Error) || r.Native != "" {
			return Invalid("/error")
		}
	} else if !(action == Merge && mergeNative.matches(r.Native) || action == Post && slackNative.matches(r.Native) || action == Run && r.Native == "") {
		return Invalid("/native")
	}
	for _, s := range []string{r.Evidence, r.Text} {
		if s != "" && !(field{max: maxNote}).matches(s) {
			return Invalid("/result")
		}
	}
	return nil
}

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// record projects a journal entry into Signal's fixed envelope. Local-only data (including the OS label and
// caller-supplied context and display-only names) never crosses this boundary.
func record(id string, e *frame, s *proposalState, who SignalIdentity) ([]byte, error) {
	if !ValidID(who.Person, "act") {
		return nil, Invalid("/metadata/person")
	}
	m := Object{"proposal": s.ID, "person": who.Person, "action": string(s.Type)}
	sealed := Object{}
	subject := "signal:actor:" + who.Person
	switch s.Type {
	case Merge:
		if n, ok := s.Params["number"].(int64); ok {
			subject = "github:pr:" + text(s.Params, "repo") + "#" + strconv.FormatInt(n, 10)
		}
	case Post:
		if s.Slack != nil {
			if public := s.Slack.publicSubject(s.Params); public != "" {
				subject = public
			}
		}
	case Run:
		if !actorHandle.matches(who.Handle) || !ValidID(s.RunnerID, "run") {
			return nil, Invalid("/subject")
		}
		subject = "agent-run:" + who.Handle + ":" + strings.TrimPrefix(s.RunnerID, "run_")
	}
	if !contextName.matches(subject) {
		return nil, Invalid("/subject")
	}
	recordType := "app.action." + e.Kind
	switch e.Kind {
	case "proposed":
		m["params_hash"], m["expires_at"] = s.ParamsHash, timestamp(s.ExpiresAt)
		proposer := Object{"agent": s.Agent, "verified": s.Peer.RunID != ""}
		setText(proposer, "session", s.Session)
		setText(proposer, "runner_id", s.Peer.RunID)
		m["proposer"] = proposer
		setText(m, "runner_id", s.RunnerID)
		sealed = Object{"params": s.Params, "salt": s.Salt, "reason": s.Reason}
	case "approved":
		f := e.Frozen
		m["approved_params_hash"], m["approved_hash"] = f.ParamsHash, f.Hash
		m["edited"] = []any{}
		sealed = Object{"params": f.Params, "salt": s.Salt, "origin": originValue(f.Origin), "account": f.Account, "destination": f.Destination}
		if f.OriginalModel != "" {
			m["edited"] = []any{"model"}
			sealed["original_model"] = f.OriginalModel
		}
	case "declined", "withdrawn", "expired", "outcome_unknown":
		m["reason"] = e.Reason
		setText(sealed, "note", e.Note)
	case "started":
		m["approved_hash"] = s.Frozen.Hash
	case "executed", "failed":
		m["confirmed_by"] = e.Result.ConfirmedBy
		if e.Kind == "failed" {
			m["error"] = e.Result.Error
		} else if s.Type == Run {
			m["runner_id"] = s.RunnerID
		} else {
			m["native"] = e.Result.Native
		}
		setText(sealed, "evidence", e.Result.Evidence)
		setText(sealed, "result", e.Result.Text)
	default:
		if e.Runner == nil || s.Type != Run {
			return nil, Invalid("/type")
		}
		recordType = "app.runner." + strings.TrimPrefix(e.Kind, "runner_")
		m["runner_id"] = s.RunnerID
		switch e.Runner.Kind {
		case RunnerWaiting:
			sealed["question"] = e.Runner.Text
		case RunnerStalled:
			sealed["last_output"] = e.Runner.Text
		case RunnerFinished:
			sealed["report"] = e.Runner.Text
		case RunnerFailed:
			m["error"] = e.Runner.Error
			setText(sealed, "detail", e.Runner.Text)
		}
		if e.Runner.Kind != RunnerClosed && e.Runner.Kind != RunnerWorking {
			m["from"] = "runner"
		}
	}
	r := Object{"version": int64(1), "key": "beacon:" + id + ":" + strconv.FormatInt(e.Seq, 10),
		"type": recordType, "subject": subject, "occurred_at": timestamp(time.UnixMilli(e.At)), "metadata": m}
	if len(sealed) > 0 {
		r["sealed"] = Object{"level": "owner+agents", "replay_nonce": e.Nonce, "metadata": sealed}
	}
	b, err := Canonical(r)
	if err != nil || len(b) > maxRecord {
		return nil, Invalid("/body")
	}
	return b, nil
}

func setText(o Object, key, s string) {
	if s != "" {
		o[key] = s
	}
}
