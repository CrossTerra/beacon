package review

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"golang.org/x/sys/unix"
)

// TestBrowserRegistersThroughThePageCSP drives Chrome's virtual authenticator
// through /passkeys/new. The ceremony posts with fetch, so this fails if
// connect-src does not allow 'self'.
func TestBrowserRegistersThroughThePageCSP(t *testing.T) {
	lockReviewPort(t)
	lns := listenReview(t)
	dir := filepath.Join(t.TempDir(), "state")
	code, err := passkey.Issue(dir, time.Now())
	must(t, err)
	j, err := approval.OpenJournal(dir, time.Now(), "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	s := New(makeOrigin(t, approval.LocalOrigin, ""), j, time.Now)
	s.SetState(dir)
	srv := &httpServer{s}
	for _, ln := range lns {
		go srv.Serve(ln)
	}
	t.Cleanup(func() {
		for _, ln := range lns {
			ln.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WSURLReadTimeout(60*time.Second),
	)
	if chrome := os.Getenv("BEACON_TEST_CHROME"); chrome != "" {
		opts = append(opts, chromedp.ExecPath(chrome))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()
	taskCtx, taskCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(string, ...any) {}))
	defer taskCancel()
	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(addVirtualAuthenticator)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(taskCtx, chromedp.Navigate("http://localhost:7317/passkeys/new")); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(taskCtx, `document.querySelector('form[action="/passkeys/new"]') && document.querySelector('form[action="/passkeys/new"]').dataset.bound === "1"`); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, err := runtime.Evaluate(`document.querySelector('input[name=code]').value = ` + strconv.Quote(code) + `; document.querySelector('form[action="/passkeys/new"] button').click()`).WithUserGesture(true).Do(ctx)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(taskCtx, `document.getElementById("ceremony-go") !== null`); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, err := runtime.Evaluate(`document.getElementById("ceremony-go").click()`).WithUserGesture(true).Do(ctx)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(taskCtx, `document.getElementById("registered") !== null`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "passkeys.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "write-vault.json")); err != nil {
		t.Fatal(err)
	}
}

type httpServer struct{ h *Site }

func (s httpServer) Serve(ln net.Listener) { http.Serve(ln, s.h) }

func listenReview(t *testing.T) []net.Listener {
	t.Helper()
	var opened []net.Listener
	closeAll := func() {
		for _, ln := range opened {
			ln.Close()
		}
	}
	for _, addr := range []string{"127.0.0.1:7317", "[::1]:7317"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll()
			t.Fatal(err)
		}
		opened = append(opened, ln)
	}
	return opened
}

func lockReviewPort(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "beacon-test-7317.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	})
}

func waitFor(ctx context.Context, expr string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			var html string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
			return errors.New("timed out: " + html)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func addVirtualAuthenticator(ctx context.Context) error {
	if err := webauthn.Enable().Do(ctx); err != nil {
		return err
	}
	var res struct {
		AuthenticatorID string `json:"authenticatorId"`
	}
	err := cdp.Execute(ctx, "WebAuthn.addVirtualAuthenticator", map[string]any{
		"options": map[string]any{
			"protocol":                    "ctap2",
			"ctap2Version":                "ctap2_1",
			"transport":                   "internal",
			"hasResidentKey":              true,
			"hasUserVerification":         true,
			"isUserVerified":              true,
			"automaticPresenceSimulation": true,
			"hasPrf":                      true,
			"hasHmacSecret":               true,
			"defaultBackupEligibility":    true,
			"defaultBackupState":          true,
		},
	}, &res)
	if err == nil && res.AuthenticatorID != "" {
		return nil
	}
	_, err = webauthn.AddVirtualAuthenticator(&webauthn.VirtualAuthenticatorOptions{
		Protocol:                    webauthn.AuthenticatorProtocolCtap2,
		Ctap2version:                webauthn.Ctap2versionCtap21,
		Transport:                   webauthn.AuthenticatorTransportInternal,
		HasResidentKey:              true,
		HasUserVerification:         true,
		IsUserVerified:              true,
		AutomaticPresenceSimulation: true,
		HasPrf:                      true,
		DefaultBackupEligibility:    true,
		DefaultBackupState:          true,
	}).Do(ctx)
	return err
}
