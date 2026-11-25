package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
)

func previewCLI() (*app, *bytes.Buffer, *bytes.Buffer) {
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return &app{stdout: out, stderr: stderr, now: time.Now,
		keychain:  func() (keychain.Store, error) { panic("preview must not touch the keychain") },
		transport: func([]string) http.RoundTripper { panic("preview must not use collector egress") },
	}, out, stderr
}

func previewTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "bg-") // short enough for a Unix socket on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startPreview(t *testing.T, dir string, origins ...approval.Origin) (*http.Client, func()) {
	t.Helper()
	origin, _ := approval.ParseOrigin(approval.LocalOrigin, "")
	if len(origins) > 0 {
		origin = origins[0]
	}
	must(t, localstate.Save(dir, origin))
	a, _, stderr := previewCLI()
	ready := make(chan string, 1)
	a.approvalListeners = func() ([]net.Listener, error) {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ready <- l.Addr().String()
		return []net.Listener{l}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		code = a.main(ctx, []string{"serve", "--state-dir", dir})
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
			if code != 0 || stderr.Len() != 0 {
				t.Errorf("preview did not stop cleanly (%d): %s", code, stderr)
			}
		case <-time.After(10 * time.Second):
			t.Error("preview did not stop")
		}
	})
	t.Cleanup(stop)
	var addr string
	select {
	case addr = <-ready:
	case <-done:
		t.Fatalf("preview exited during startup (%d): %s", code, stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("preview did not bind its listener")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
	}}}
	t.Cleanup(client.CloseIdleConnections)
	return client, stop
}

func previewGET(t *testing.T, client *http.Client, link string, status int, cookie *http.Cookie) (string, []*http.Cookie) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != status {
		t.Fatalf("review response: status %s, want %d; read: %v", res.Status, status, err)
	}
	return string(b), res.Cookies()
}

func TestGoPreviewCLIFromSocketToBrowserAndRestart(t *testing.T) {
	dir := previewTemp(t)
	client, stop := startPreview(t, dir)
	previewGET(t, client, approval.LocalOrigin+"/sign-in", 200, nil)
	a, out, stderr := previewCLI()
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		stderr.Reset()
		if code := a.main(t.Context(), args); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr)
		}
		return strings.TrimSpace(out.String())
	}
	run("act", "--state-dir", dir, "types")
	a.stdin = strings.NewReader(`{
	  "type":"slack.post","params":{"workspace":"T1","channel":"C1","thread_ts":null,"text":"Fixture draft."},
	  "reason":"Fixture review.","agent":{"name":"bosun"}
	}`)
	var reply struct {
		ID        string
		ReviewURL string `json:"review_url"`
	}
	if err := json.Unmarshal([]byte(run("act", "--state-dir", dir, "propose")), &reply); err != nil ||
		!approval.ValidID(reply.ID, "apr") || reply.ReviewURL != approval.LocalOrigin+"/p/"+reply.ID {
		t.Fatalf("bad propose reply: %+v: %v", reply, err)
	}
	if b, _ := previewGET(t, client, reply.ReviewURL, 401, nil); strings.Contains(b, "Fixture draft") {
		t.Fatal("unsigned-in browser got proposal text")
	}
	link := run("open", "--state-dir", dir)
	_, cookies := previewGET(t, client, link, 303, nil)
	if len(cookies) != 1 {
		t.Fatal("no session cookie")
	}
	if b, _ := previewGET(t, client, reply.ReviewURL, 200, cookies[0]); !strings.Contains(b, "Fixture draft.") {
		t.Fatal("browser did not render the proposal")
	}
	previewGET(t, client, link, 401, nil) // one use
	unused := run("open", "--state-dir", dir)
	stop()
	if _, err := os.Lstat(filepath.Join(dir, "run", "act.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not cleaned up: %v", err)
	}
	client, stop = startPreview(t, dir)
	previewGET(t, client, reply.ReviewURL, 401, cookies[0])
	previewGET(t, client, unused, 401, nil)
	if got := run("act", "--state-dir="+dir, "get", reply.ID); !strings.Contains(got, `"status":"pending"`) {
		t.Fatalf("replay lost proposal: %s", got)
	}
	if got := run("act", "--state-dir", dir, "withdraw", reply.ID); !strings.Contains(got, `"status":"withdrawn"`) {
		t.Fatalf("withdraw failed: %s", got)
	}
	if code := a.main(t.Context(), []string{"act", "--state-dir", dir, "quit"}); code != 2 {
		t.Fatalf("Go client accepted quit (exit %d)", code)
	}
	stop()
}

func TestGoPreviewUsesTheConfiguredTailnetOrigin(t *testing.T) {
	const origin = "https://machine.tail123.ts.net"
	dir := previewTemp(t)
	o, err := approval.ParseOrigin(origin, "fixture@example.test")
	must(t, err)
	client, _ := startPreview(t, dir, o)
	previewGET(t, client, approval.LocalOrigin+"/sign-in", http.StatusFound, nil)
	a, stdout, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"open", "--state-dir", dir}); code != 0 ||
		!strings.HasPrefix(stdout.String(), origin+"/open?code=") {
		t.Fatalf("open did not use the configured origin (exit %d): %s", code, stderr)
	}
}

func TestGoServiceRefusesAnUnconfiguredPathAndUnmigratedState(t *testing.T) {
	// An isolated fake home also exercises the standard Darwin path without probing the real one.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	dir := previewTemp(t)
	must(t, os.Mkdir(filepath.Join(dir, "app"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "app", "journal.sqlite"), []byte("not read"), 0o600))
	a, _, stderr := previewCLI()
	a.approvalListeners = func() ([]net.Listener, error) { panic("refused command must not start listeners") }
	for _, args := range [][]string{{"serve"}, {"open"}, {"serve", "--state-dir", dir}, {"act", "--state-dir", dir, "types"}} {
		if code := a.main(t.Context(), args); code == 0 {
			t.Fatalf("accepted %v: %s", args, stderr)
		}
	}
	o, _ := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, localstate.Save(dir, o))
	for _, cmd := range []string{"setup", "serve"} {
		stderr.Reset()
		if code := a.main(t.Context(), []string{cmd, "--state-dir", dir}); code == 0 || !strings.Contains(stderr.String(), "--migrate-swift") {
			t.Fatalf("%s with unmigrated Swift journal: %s", cmd, stderr)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "journal.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote a Go journal over unmigrated state", err)
	}
}

func TestGoServiceIgnoresOrphanedActSettings(t *testing.T) {
	dir := previewTemp(t)
	legacy := filepath.Join(dir, "act.json")
	const fixture = "old client trust settings; not read by Go"
	must(t, os.WriteFile(legacy, []byte(fixture), 0o600))
	a, _, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir, "--origin", "localhost"}); code != 0 {
		t.Fatalf("setup with only act.json exited %d: %s", code, stderr)
	}
	origin, err := localstate.Load(dir)
	must(t, err)
	client, stop := startPreview(t, dir, origin)
	previewGET(t, client, approval.LocalOrigin+"/sign-in", 200, nil)
	stop()
	data, err := os.ReadFile(legacy)
	must(t, err)
	if string(data) != fixture {
		t.Fatal("Go rewrote act.json")
	}
	if _, err := os.Lstat(filepath.Join(dir, "swift-archive")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected archive for orphaned act.json: %v", err)
	}
}

func TestOpenPrintsOnlyCanonicalLinks(t *testing.T) {
	code := strings.Repeat("A", 43) // fixture, not a live code
	for _, base := range []string{approval.LocalOrigin, "https://machine.tail123.ts.net", "https://example.test", "http://127.0.0.1:7317"} {
		link := base + "/open?code=" + code
		b, err := json.Marshal(approval.Object{"code": code, "url": link})
		if err != nil {
			t.Fatal(err)
		}
		got, err := openLink(b)
		if base == approval.LocalOrigin || strings.HasSuffix(base, ".ts.net") {
			if err != nil || got != link {
				t.Fatalf("rejected canonical URL: %v", err)
			}
		} else if err == nil {
			t.Fatalf("returned a noncanonical URL: %s", base)
		}
	}
}

func TestGoEgressListsConfiguredProvidersWithoutCredentials(t *testing.T) {
	dir := previewTemp(t)
	settings := `{"github":{"origin":"https://github.fixture.test","user_id":123},"slack":{"workspace":"TWORKSPACE","user_id":"UPERSON"}}`
	if err := os.WriteFile(filepath.Join(dir, "action-providers.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"egress", "--state-dir", dir}); code != 0 {
		t.Fatalf("egress exited %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "github.fixture.test:443 ") || !strings.HasPrefix(lines[1], "slack.com:443 ") {
		t.Fatalf("wrong Go provider egress: %s", out)
	}
}
