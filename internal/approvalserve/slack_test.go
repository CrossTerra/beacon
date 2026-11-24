package approvalserve

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
)

const (
	slackRead      = "xoxp-fixture-serve-read"
	slackWrite     = "xoxp-fixture-serve-write"
	slackExactText = "*Review* <https://example.test|the docs> <@UOTHER|claimed> <!channel>\nCafe\u0301\t👍"
	slackNative    = "slack:message:CGENERAL:1800000000.000002"
	slackScopes    = "channels:read,groups:read,im:read,mpim:read,users:read,channels:history,groups:history,im:history,mpim:history"
)

type slackProviderFixture struct {
	posts       atomic.Int32
	reads       atomic.Int32
	hide        atomic.Bool
	missingRoot atomic.Bool
	body        atomic.Pointer[approval.Object]
}

func (f *slackProviderFixture) transport(t *testing.T, first *gitProviderFixture) http.RoundTripper {
	return transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "slack.com" {
			t.Error("contacted an unconfigured Slack origin")
			return fixtureTransportAnswer(500, "{}")
		}
		auth := r.Header.Get("Authorization")
		read, write := auth == "Bearer "+slackRead, auth == "Bearer "+slackWrite
		if !read && !write {
			t.Error("used the wrong fixture token")
			return fixtureTransportAnswer(401, "{}")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/auth.test":
			f.reads.Add(1)
			res, _ := fixtureTransportAnswer(200, `{"ok":true,"team_id":"TWORKSPACE","user_id":"UPERSON","team":"Fixture Workspace","user":"fixture-user"}`)
			scopes := slackScopes
			if write {
				scopes += ",chat:write"
			}
			res.Header.Set("X-OAuth-Scopes", scopes)
			return res, nil
		case "GET /api/conversations.info":
			if r.URL.Query().Get("channel") != "CGENERAL" {
				t.Error("looked up the wrong Slack channel")
			}
			return fixtureTransportAnswer(200, `{"ok":true,"channel":{"id":"CGENERAL","name":"general","is_channel":true,"is_private":false,"is_member":true}}`)
		case "GET /api/conversations.replies":
			q := r.URL.Query()
			if q.Get("ts") != "1799999880.000001" || q.Get("channel") != "CGENERAL" {
				t.Error("read outside the approved Slack thread")
			}
			if q.Get("oldest") == "" {
				if f.missingRoot.Load() {
					return fixtureTransportAnswer(200, `{"ok":true,"messages":[]}`)
				}
				return fixtureTransportAnswer(200, `{"ok":true,"messages":[{"type":"message","ts":"1799999880.000001","text":"Root preview\nNot part of the preview"}]}`)
			}
			if !read || q.Get("include_all_metadata") != "true" {
				t.Error("recovery used the wrong credential or did not request metadata")
			}
			body := f.body.Load()
			if body == nil || f.hide.Load() {
				return fixtureTransportAnswer(200, `{"ok":true,"messages":[]}`)
			}
			wire, err := approval.Canonical(approval.Object{"ok": true, "messages": []any{approval.Object{
				"type": "message", "user": "UPERSON", "ts": "1800000000.000002", "channel": (*body)["channel"],
				"thread_ts": (*body)["thread_ts"], "text": (*body)["text"], "metadata": (*body)["metadata"],
			}}})
			if err != nil {
				t.Error("could not encode fixture read-back")
				return fixtureTransportAnswer(500, "{}")
			}
			return fixtureTransportAnswer(200, string(wire))
		case "POST /api/chat.postMessage":
			f.posts.Add(1)
			if !write || first.puts.Load() != 1 || r.GetBody != nil {
				t.Error("post did not use the write token once and in display order")
			}
			data, err := io.ReadAll(r.Body)
			r.Body.Close()
			body, bad := approval.ParseObject(data)
			meta, _ := body["metadata"].(approval.Object)
			payload, _ := meta["event_payload"].(approval.Object)
			op, _ := body["client_msg_id"].(string)
			if err != nil || bad != nil || body["text"] != slackExactText || body["channel"] != "CGENERAL" || body["thread_ts"] != "1799999880.000001" ||
				body["unfurl_links"] != false || body["unfurl_media"] != false || meta["event_type"] != "beacon_post" || payload["op"] != op ||
				!regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(op) {
				t.Error("Slack did not receive the frozen source, destination and operation marker")
			}
			f.body.Store(&body)
			return nil, io.ErrUnexpectedEOF
		}
		t.Errorf("unexpected Slack fixture call: %s %s", r.Method, r.URL.Path)
		return fixtureTransportAnswer(500, "{}")
	})
}

func TestServeGatesMixedBatchAndRecoversSlackWithoutResending(t *testing.T) {
	opts := options(t)
	must(t, os.WriteFile(filepath.Join(opts.StateDir, "action-providers.json"), []byte(`{
		"github":{"origin":"https://github.fixture.test","user_id":123},
		"slack":{"workspace":"TWORKSPACE","user_id":"UPERSON"}}`), 0o600))
	key, prf := registerFixturePasskey(t, opts.StateDir)
	defer clear(prf)
	gh, slack := new(gitProviderFixture), new(slackProviderFixture)
	slack.hide.Store(true)
	opts.ProviderTransport = func(hosts []string) http.RoundTripper {
		if len(hosts) == 1 && hosts[0] == "slack.com:443" {
			return slack.transport(t, gh)
		}
		return gh.transport(t)
	}
	b, stop := launchBrowser(t, opts)
	setup := func(service, read, write string) url.Values {
		t.Helper()
		_, page := b.request(http.MethodPost, "/settings/begin", url.Values{"csrf": {b.csrf}, "service": {service}}, 200)
		if service == "slack" && (!strings.Contains(page, "slack:user:UPERSON") || !strings.Contains(page, "TWORKSPACE")) {
			t.Fatal("Slack settings assertion did not show the fixed binding")
		}
		form := b.form(key, prf, page)
		form.Set("read_token", read)
		form.Set("write_token", write)
		return form
	}
	b.request(http.MethodPost, "/settings/save", setup("github", mergeRead, mergeWrite), 200)
	slackSettings := setup("slack", slackRead, slackWrite)
	b.request(http.MethodPost, "/approve", slackSettings, 409) // a settings assertion isn't an approval
	if slack.reads.Load() != 0 {
		t.Fatal("settings used the tokens before assertion")
	}
	b.request(http.MethodPost, "/settings/save", slackSettings, 200)
	mergeID := proposeMerge(t, opts.StateDir)
	params := approval.Object{"workspace": "TWORKSPACE", "channel": "CGENERAL", "thread_ts": "1799999880.000001", "text": slackExactText}
	post := approval.Object{"op": "propose", "type": "slack.post", "agent": approval.Object{"name": "fixture"}, "reason": "fixture post", "params": params}
	propose := func() approval.Object {
		t.Helper()
		wire, err := actsocket.Call(t.Context(), opts.StateDir, post, 0)
		must(t, err)
		p, err := approval.ParseObject(wire)
		must(t, err)
		return p
	}
	params["origin"] = "https://example.test"
	if propose()["field"] != "/params/origin" {
		t.Fatal("socket accepted a proposer-supplied origin")
	}
	delete(params, "origin")
	params["workspace"] = "TOTHER"
	if propose()["error"] != "unavailable" {
		t.Fatal("socket accepted a different workspace")
	}
	params["workspace"] = "TWORKSPACE"
	slackID, _ := propose()["id"].(string)
	if !approval.ValidID(slackID, "apr") {
		t.Fatal("verified Slack intake refused the draft")
	}
	_, pending := b.request(http.MethodGet, "/", nil, 200)
	if !strings.Contains(pending, "Post in Slack #general · public · Fixture Workspace") {
		t.Fatal("missing verified destination on the pending list")
	}
	selection := url.Values{"csrf": {b.csrf}, "id": {slackID, mergeID}} // reversed form order is not display order
	slack.missingRoot.Store(true)
	_, denied := b.request(http.MethodPost, "/freeze", selection, 409)
	if !strings.Contains(denied, "thread_not_found") || strings.Contains(denied, `id="ceremony"`) {
		t.Fatal("unavailable root did not block batch review")
	}
	slack.missingRoot.Store(false)
	freeze := func() string {
		t.Helper()
		_, page := b.request(http.MethodPost, "/freeze", selection, 200)
		pos := strings.Index(page, mergeID)
		if pos < 0 || strings.Index(page, slackID) <= pos {
			t.Fatal("batch did not freeze in display order")
		}
		for _, fact := range []string{"#general", "Root preview", "Fixture Workspace", "1799999880.000001",
			"[@channel — notifies everyone in the channel]", "<details open><summary>Exact Slack mrkdwn</summary>", "&lt;!channel&gt;"} {
			if !strings.Contains(page, fact) {
				t.Fatal("missing Slack review field", fact)
			}
		}
		if strings.Contains(page, "Not part of the preview") || strings.Contains(page, `href="https://example.test`) {
			t.Fatal("review leaked the rest of the root or made a draft link interactive")
		}
		return page
	}
	form := b.form(key, prf, freeze())
	form.Set("signature", "AA")
	b.request(http.MethodPost, "/approve", form, 401)
	sealed := filepath.Join(opts.StateDir, "write-slack.sealed")
	good, err := os.ReadFile(sealed)
	must(t, err)
	must(t, os.WriteFile(sealed, []byte("not an authenticated Slack token"), 0o600))
	_, denied = b.request(http.MethodPost, "/approve", b.form(key, prf, freeze()), 409)
	if !strings.Contains(denied, "credential_unavailable") {
		t.Fatal("wrong refusal for an unreadable sealed credential")
	}
	b.waitFor(mergeID, "pending (version 1)")
	b.waitFor(slackID, "pending (version 1)")
	if gh.puts.Load() != 0 || slack.posts.Load() != 0 {
		t.Fatal("a write escaped a failed assertion or unseal of the second token")
	}
	must(t, os.WriteFile(sealed, good, 0o600))
	form = b.form(key, prf, freeze())
	b.request(http.MethodPost, "/approve", form, 200)
	b.waitFor(mergeID, "github:merge:"+mergeSHA)
	b.waitFor(slackID, "Read again")
	b.request(http.MethodPost, "/read-back", url.Values{"csrf": {b.csrf}, "id": {slackID}}, 303)
	b.request(http.MethodPost, "/approve", form, 409)
	stop()

	// After startup recovery can see the post, it confirms it using the UUID that survived restart.
	slack.hide.Store(false)
	b, stop = launchBrowser(t, opts)
	b.waitFor(slackID, slackNative)
	b.waitFor(slackID, "read_back")
	stop()
	if gh.puts.Load() != 1 || slack.posts.Load() != 1 {
		t.Fatalf("want one write to each provider, got %d merges and %d posts", gh.puts.Load(), slack.posts.Load())
	}
	for _, name := range []string{"journal.log", "unattended.kdbx", "unattended.keyx", "passkeys.json", "write-vault.json", "write-slack.sealed"} {
		body, err := os.ReadFile(filepath.Join(opts.StateDir, name))
		must(t, err)
		for _, needle := range [][]byte{[]byte(slackRead), []byte(slackWrite), prf, []byte(base64.StdEncoding.EncodeToString(prf)), []byte(base64.RawURLEncoding.EncodeToString(prf))} {
			if bytes.Contains(body, needle) {
				t.Fatalf("%s stored a raw credential or PRF output", name)
			}
		}
	}
}
