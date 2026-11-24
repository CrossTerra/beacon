package approval

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

var who = SignalIdentity{Person: "act_01m6a6ft00e40r2081040g2081", Handle: "winton"}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func obj(v any) Object { o, _ := v.(Object); return o }

func fixture(t *testing.T, name string) []any {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	must(t, err)
	v, err := parseJSON(b)
	must(t, err)
	return v.([]any)
}

func TestCanonicalHashes(t *testing.T) {
	for _, v := range fixture(t, "hashes") {
		c := obj(v)
		t.Run(text(c, "note"), func(t *testing.T) {
			b, err := Canonical(c["value"])
			must(t, err)
			if string(b) != text(c, "canonical") {
				t.Fatalf("canonical JSON = %q, want %q", b, c["canonical"])
			}
			h, err := digest(obj(c["value"]))
			must(t, err)
			if h != c["hash"] {
				t.Fatalf("digest = %s, want %s", h, c["hash"])
			}
			roundTrip, err := parseJSON(b)
			must(t, err)
			again, err := Canonical(roundTrip)
			must(t, err)
			if !bytes.Equal(again, b) {
				t.Fatal("canonical value did not round trip")
			}
		})
	}
}

func TestStrictJSON(t *testing.T) {
	for _, s := range []string{`1.0`, `1e0`, `-1E2`, `9007199254740992`, `-9007199254740992`, `01`, `{"a":1,"\u0061":2}`,
		`"\ud800"`, `"\udc00x"`, `"\ud800\ud800"`, "\"a\x01b\"", `[1,]`, `{}x`, ``, `nul`, `"\x41"`, "\"\xff\"",
		strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)} {
		if _, err := parseJSON([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	v, err := parseJSON([]byte(` [9007199254740991, -0, "\ud83d\udc4d", "\\ud800", {"b": null}] `))
	must(t, err)
	b, err := Canonical(v)
	must(t, err)
	if string(b) != `[9007199254740991,0,"👍","\\ud800",{"b":null}]` {
		t.Fatalf("decoded %q", b)
	}
}

func TestPublicID(t *testing.T) {
	early, err := newID("apr", t0)
	must(t, err)
	late, err := newID("apr", t0.Add(time.Millisecond))
	must(t, err)
	if early >= late || !ValidID(early, "apr") || ValidID(early, "run") || !ValidID("apr_01m7wyd4m0f858m2ga1850m2ga", "apr") {
		t.Fatalf("invalid generated ids %q %q", early, late)
	}
	for _, s := range []string{"81m7wyd4m0f858m2ga1850m2ga", "01m7wyd4m0f858m2ga1850m2g", "01m7wyd4m0f858m2ga1850m2gu", "01m7wyd4m0a858m2ga1850m2ga"} {
		if ValidID("apr_"+s, "apr") {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestActionSchemas(t *testing.T) {
	for _, c := range []struct {
		a     Action
		p     Object
		field Invalid
	}{
		{Merge, mergeRequest().Params, ""},
		{Post, Object{"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "hi\n"}, ""},
		{Post, Object{"workspace": "T1", "channel": "C1", "text": "hi"}, "/params/thread_ts"},
		{Post, Object{"workspace": "T1", "channel": "C1", "thread_ts": "1.2", "text": "hi"}, "/params/thread_ts"},
		{Post, Object{"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": ""}, "/params/text"},
		{Run, Object{"checkout": "beacon", "prompt": strings.Repeat("👍", 32_000), "model": "a/b"}, ""},
		{Run, Object{"checkout": "beacon", "prompt": strings.Repeat("👍", 32_001), "model": "a/b"}, "/params/prompt"},
		{Run, Object{"checkout": "Beacon", "prompt": "p", "model": "a/b"}, "/params/checkout"},
		{Run, Object{"checkout": "beacon", "prompt": "p", "model": "opus"}, "/params/model"},
	} {
		err := validate(c.p, "/params", paramsSchemas[c.a])
		if c.field == "" && err != nil || c.field != "" && err != c.field {
			t.Errorf("%s validate = %#v, want %q", c.a, err, c.field)
		}
	}
	for k, bad := range (Object{"head": "ab", "method": "fast-forward", "number": int64(0), "extra": true}) {
		p := mergeRequest().Params
		p[k] = bad
		if err := validate(p, "/params", paramsSchemas[Merge]); err != Invalid("/params/"+k) {
			t.Errorf("accepted invalid %s: %#v", k, err)
		}
	}
	for _, end := range []string{"\n", "\r", "\r\n", "\u2028", "\u0085"} {
		for _, k := range []string{"repo", "base", "head", "method"} {
			p := mergeRequest().Params
			p[k] = p[k].(string) + end
			if err := validate(p, "/params", paramsSchemas[Merge]); err != Invalid("/params/"+k) {
				t.Errorf("accepted trailing terminator in %s", k)
			}
		}
		r := Object{"op": "propose", "type": string(Merge), "params": mergeRequest().Params, "reason": "r", "agent": Object{"name": "bosun"}}
		for _, k := range []string{"name", "session"} {
			a := obj(r["agent"])
			a[k] = "bosun" + end
			if _, err := ParseRequest(r); err != Invalid("/agent/"+k) {
				t.Errorf("accepted trailing terminator in agent %s", k)
			}
			a[k] = "bosun"
		}
	}
}

// Play the fixture through production journal transitions, fixing ids, salts, times and replay nonces only.
// Slack without a trusted intake never goes on a shared subject.
func fixtureJournal(t *testing.T) (*Journal, []Object) {
	t.Helper()
	dir := scratch(t)
	var replayNonce string
	hooks := journalHooks{func(prefix string, at time.Time) (string, error) {
		if prefix == "jrn" {
			return "jrn_01m6a6ft00e81840g2081040g2", nil
		}
		return newID(prefix, at)
	}, func() string { return replayNonce }, syncJournal}
	j, err := openJournal(dir, iso(t, "2026-11-20T16:59:00.000Z"), "", hooks)
	must(t, err)
	t.Cleanup(func() { j.Close() })
	var expected []Object
	for _, v := range fixture(t, "journal") {
		r := obj(obj(v)["body"])
		kind := text(r, "type")
		m, sealed := obj(r["metadata"]), obj(r["sealed"])
		s := obj(sealed["metadata"])
		id, a := text(m, "proposal"), Action(text(m, "action"))
		at := iso(t, text(r, "occurred_at"))
		replayNonce = text(sealed, "replay_nonce")
		if replayNonce == "" {
			replayNonce = strings.Repeat("0", 32)
		}
		switch kind {
		case "app.action.proposed":
			pr := obj(m["proposer"])
			request := Request{Type: a, Params: obj(s["params"]), Agent: text(pr, "agent"), Session: text(pr, "session"), Reason: text(s, "reason")}
			must(t, request.check())
			p := Proposal{Request: request, ID: id, Salt: text(s, "salt"), Status: "pending", Version: 1,
				RunnerID: text(m, "runner_id"), ProposedAt: at, ExpiresAt: at.Add(Lifetime)}
			p.ParamsHash, err = paramsDigest(a, p.Params, p.Salt)
			must(t, err)
			_, err = j.add(p)
			must(t, err)
		case "app.action.approved":
			p, _, err := j.Get(id, at)
			must(t, err)
			selected := ""
			if s["original_model"] != nil {
				selected = text(obj(s["params"]), "model")
			}
			f, err := Freeze(p.Proposal, Binding{text(s, "origin"), text(s, "account"), obj(s["destination"])}, selected)
			must(t, err)
			must(t, j.Approve(f, at))
		case "app.action.started":
			_, err := j.Start(id, at)
			must(t, err)
		case "app.action.executed", "app.action.failed":
			must(t, j.SetResult(id, Result{text(m, "confirmed_by"), text(m, "native"), text(m, "error"), text(s, "evidence"), text(s, "result")}, at))
		case "app.action.outcome_unknown":
			must(t, j.Unknown(id, text(m, "reason"), at))
		case "app.action.expired":
			must(t, j.Shutdown(at))
			must(t, j.Close())
			j, err = openJournal(dir, at, "", hooks)
			must(t, err)
		case "app.action.withdrawn":
			_, err := j.Withdraw(id, "", at)
			must(t, err)
		case "app.action.declined":
			must(t, j.Decline(id, text(s, "note"), at))
		case "app.runner.working", "app.runner.waiting", "app.runner.finished", "app.runner.closed":
			event := RunnerEvent{Kind: RunnerKind("runner_" + strings.TrimPrefix(kind, "app.runner."))}
			switch event.Kind {
			case RunnerWaiting:
				event.Text = text(s, "question")
			case RunnerFinished:
				event.Text = text(s, "report")
			}
			must(t, j.RecordRunner(id, event, at))
		default:
			t.Fatalf("unhandled fixture %s", kind)
		}
		switch a {
		case Merge:
			r["subject"] = "github:pr:CrossTerra/signal#140"
		case Post:
			r["subject"] = "signal:actor:" + who.Person
		}
		expected = append(expected, r)
	}
	return j, expected
}

func iso(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	must(t, err)
	return at
}
