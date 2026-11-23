// SlackFixture is a loopback Slack Web API for Beacon's Swift tests. It uses httptest's TLS certificate, pinned
// only by the test delegate. The parent closes stdin to stop it. No real Slack credentials are accepted.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	token  = "Bearer xoxp-fixture-token"
	parent = "1799999900.000100"
	posted = "1800000000.000200"
)

type object = map[string]any

type settings struct {
	User        string   `json:"user"`
	Workspace   string   `json:"workspace"`
	Bot         bool     `json:"bot"`
	Channel     object   `json:"channel"`
	Parent      object   `json:"parent"`
	Members     []string `json:"members"`
	MissingRoot bool     `json:"missing_root"`
	RateMethod  string   `json:"rate_method"`
	RetryAfter  int      `json:"retry_after"`
	PostMode    string   `json:"post_mode"`
	Redirect    string   `json:"redirect"`
	HidePosts   bool     `json:"hide_posts"`
	Echo        object   `json:"echo"`
	Duplicate   bool     `json:"duplicate"`
	Paginate    bool     `json:"paginate"`
	// Metadata is how chat.postMessage treats message metadata: "" keeps it (returned only with
	// include_all_metadata=true), "drop" accepts the post and discards it, "refuse" answers
	// metadata_must_be_sent_from_app as Slack documents for a token that may not post metadata.
	Metadata string `json:"metadata"`
	// DropClientID discards client_msg_id, as an API post may.
	DropClientID bool `json:"drop_client_id"`
	// Users overrides users.info by id. Names "refuse" is a missing scope, so a name lookup can fail.
	Users map[string]object `json:"users"`
	Names string            `json:"names"`
	// Needed and Provided are Slack's missing_scope fields. Scopes is the X-OAuth-Scopes header; empty means
	// every scope Beacon requires, and "omit" sends none.
	Needed   string `json:"needed"`
	Provided string `json:"provided"`
	Scopes   string `json:"scopes"`
}

type call struct {
	Method string     `json:"method"`
	Path   string     `json:"path"`
	Query  url.Values `json:"query"`
	Body   object     `json:"body"`
	Authed bool       `json:"authed"`
}

type slack struct {
	sync.Mutex
	settings settings
	calls    []call
	messages []object
}

func (s *slack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Lock()
	defer s.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(v object) { _ = json.NewEncoder(w).Encode(v) }
	refuse := func(code string) {
		body := object{"ok": false, "error": code}
		if code == "missing_scope" {
			if s.settings.Needed != "" {
				body["needed"] = s.settings.Needed
			}
			if s.settings.Provided != "" {
				body["provided"] = s.settings.Provided
			}
		}
		write(body)
	}
	if r.URL.Path == "/fixture" {
		switch r.Method {
		case http.MethodGet:
			write(object{"calls": s.calls})
		case http.MethodPut:
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&s.settings); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			write(object{"ok": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	q := r.URL.Query()
	var body object
	if method == "chat.postMessage" && r.Method == http.MethodPost {
		// Empty/invalid bodies still count as wire attempts.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	}
	auth := r.Header.Get("Authorization")
	s.calls = append(s.calls, call{Method: r.Method, Path: r.URL.Path, Query: q, Body: body, Authed: auth == token || auth == token+"-2"})
	if auth != token && auth != token+"-2" {
		refuse("invalid_auth")
		return
	}
	c := s.settings
	user := c.User
	if auth == token+"-2" {
		user = "U0OTHER"
	}
	if method == c.RateMethod {
		w.Header().Set("Retry-After", strconv.Itoa(c.RetryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if method == c.Redirect {
		http.Redirect(w, r, "/untrusted", http.StatusTemporaryRedirect)
		return
	}
	if (method == "chat.postMessage") != (r.Method == http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(method, "conversations.") && q.Get("channel") != c.Channel["id"] {
		refuse("channel_not_found")
		return
	}
	switch method {
	case "auth.test":
		if c.Scopes != "omit" {
			scopes := c.Scopes
			if scopes == "" {
				scopes = "chat:write,channels:read,groups:read,im:read,mpim:read,users:read," +
					"channels:history,groups:history,im:history,mpim:history"
			}
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		v := object{"ok": true, "team_id": c.Workspace, "team": "Fixture Workspace", "user_id": user, "user": "fixture-user"}
		if c.Bot {
			v["bot_id"] = "B0BOT"
		}
		write(v)
	case "conversations.info":
		write(object{"ok": true, "channel": c.Channel})
	case "conversations.members":
		write(object{"ok": true, "members": c.Members})
	case "users.info":
		if c.Names == "refuse" {
			refuse("missing_scope")
			return
		}
		id := q.Get("user")
		user := object{"id": id, "name": "dana", "real_name": "Dana Scully"}
		maps.Copy(user, c.Users[id])
		write(object{"ok": true, "user": user})
	case "conversations.replies", "conversations.history":
		if method == "conversations.replies" && q.Get("ts") != parent {
			refuse("thread_not_found")
			return
		}
		if method == "conversations.replies" && q.Get("oldest") == "" {
			if c.MissingRoot {
				refuse("thread_not_found")
			} else {
				write(object{"ok": true, "messages": []object{c.Parent}})
			}
			return
		}
		if c.Paginate && q.Get("cursor") == "" {
			write(object{"ok": true, "messages": []object{}, "has_more": true,
				"response_metadata": object{"next_cursor": "fake-page=="}})
			return
		}
		messages := []object{}
		if !c.HidePosts {
			for _, saved := range s.messages {
				m := maps.Clone(saved)
				if q.Get("include_all_metadata") != "true" {
					delete(m, "metadata")
				}
				maps.Copy(m, c.Echo)
				messages = append(messages, m)
				if c.Duplicate {
					another := maps.Clone(m)
					another["ts"] = "1800000000.000300"
					messages = append(messages, another)
				}
			}
		}
		write(object{"ok": true, "messages": messages})
	case "chat.postMessage":
		if body["channel"] != c.Channel["id"] || body["text"] == nil {
			refuse("invalid_arguments")
			return
		}
		if body["metadata"] != nil && c.Metadata == "refuse" {
			refuse("metadata_must_be_sent_from_app")
			return
		}
		if strings.HasPrefix(c.PostMode, "refuse:") {
			refuse(strings.TrimPrefix(c.PostMode, "refuse:"))
			return
		}
		m := object{"type": "message", "user": user, "ts": posted, "text": body["text"]}
		if !c.DropClientID {
			m["client_msg_id"] = body["client_msg_id"]
		}
		if body["metadata"] != nil && c.Metadata == "" {
			m["metadata"] = body["metadata"]
		}
		if root := body["thread_ts"]; root != nil {
			m["thread_ts"] = root
		}
		s.messages = append(s.messages, m)
		switch c.PostMode {
		case "lost":
			conn, _, err := w.(http.Hijacker).Hijack() // The request was accepted; give CFNetwork no response at all.
			if err == nil {
				_ = conn.Close()
			}
		case "unclear":
			refuse("internal_error")
		default:
			reply := maps.Clone(m)
			delete(reply, "metadata")
			write(object{"ok": true, "channel": body["channel"], "ts": posted, "message": reply})
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func stopFixture(server *httptest.Server) {
	// The parent is done, even if a cancelled client left a request body half sent. Close the listener and
	// active connections before httptest waits for handlers, so reads of those bodies can finish.
	_ = server.Config.Close()
	server.Close()
}

func main() {
	s := &slack{settings: settings{
		User: "U0PERSON", Workspace: "T0WORKSPACE", Members: []string{"U0PERSON", "U0OTHER"},
		Channel: object{"id": "C0GENERAL", "name": "general", "context_team_id": "T0WORKSPACE",
			"is_channel": true, "is_private": false, "is_archived": false, "is_member": true},
		Parent: object{"ts": parent, "type": "message", "thread_ts": parent, "text": "Parent’s first line\nNot part of the preview."},
	}}
	server := httptest.NewTLSServer(s)
	defer stopFixture(server)
	fmt.Println(server.URL + " " + base64.StdEncoding.EncodeToString(server.Certificate().Raw))
	_, _ = io.Copy(io.Discard, os.Stdin)
}
