package spike

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
)

// TestBrowser drives headless Chrome's virtual authenticator through register
// and assert with PRF, a credential without PRF, and a wrong origin.
func TestBrowser(t *testing.T) {
	lns, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	srv, err := New(Config{DataDir: t.TempDir(), Log: &log})
	if err != nil {
		for _, ln := range lns {
			_ = ln.Close()
		}
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx, lns) }()
	defer func() {
		cancel()
		<-errc
	}()

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
	taskCtx, taskCancel = context.WithTimeout(taskCtx, 90*time.Second)
	defer taskCancel()

	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(addVirtualAuthenticator)); err != nil {
		t.Fatal(err)
	}
	result, err := browserCeremony(taskCtx, "http://localhost:7317/register", "result")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "PRF is enabled") {
		t.Fatalf("register result %q", result)
	}

	first, err := browserCeremony(taskCtx, "http://localhost:7317/assert", "prf-hash")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(first)) != 64 {
		t.Fatalf("prf hash %q", first)
	}
	second, err := browserCeremony(taskCtx, "http://localhost:7317/assert", "prf-hash")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(first) != strings.TrimSpace(second) {
		t.Fatalf("hash changed: %s then %s", first, second)
	}

	if strings.Contains(log.String(), "prf_output") || strings.Contains(log.String(), "client_data_json") {
		t.Fatal("request body logged")
	}
	if !strings.Contains(log.String(), strings.TrimSpace(first)) {
		t.Fatal("PRF hash was not logged")
	}

	reason, err := browserCeremony(taskCtx, "http://localhost:7317/register?prf=0", "reason")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "PRF") {
		t.Fatalf("without PRF: %q", reason)
	}

	if err := chromedp.Run(taskCtx, chromedp.Navigate("http://127.0.0.1:7317/")); err != nil {
		t.Fatal(err)
	}
	reason, err = textOf(taskCtx, "reason")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "Host") {
		t.Fatalf("wrong host: %q", reason)
	}

	// A POST whose Origin is not the canonical origin is refused before the body
	// is read. The browser cannot forge Origin, so this is the same check a
	// page on another origin would hit.
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:7317/assert", strings.NewReader("ceremony=x"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:7317"
	req.Header.Set("Origin", "http://127.0.0.1:7317")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	refused, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(refused), "Origin") {
		t.Fatalf("wrong origin status %d body %s", resp.StatusCode, refused)
	}
}

// ceremony opens url, clicks the passkey button and returns the text of id
// after the form posts. Queries go through Evaluate: this Chrome's layout
// events do not unmarshal in the pinned DevTools client, and WaitVisible then
// never returns.
func browserCeremony(ctx context.Context, url, id string) (string, error) {
	// userGesture makes Chrome send the page's Origin. Without it a scripted
	// form submit is Origin: null, which the spike refuses.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		waitReady(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, err := runtime.Evaluate(`document.getElementById("ceremony-go").click()`).WithUserGesture(true).Do(ctx)
			return err
		}),
	); err != nil {
		return "", err
	}
	return textOf(ctx, id)
}

func textOf(ctx context.Context, id string) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	expr := `document.getElementById("` + id + `") ? document.getElementById("` + id + `").textContent : ""`
	for {
		var text string
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &text)); err != nil {
			return "", err
		}
		if strings.TrimSpace(text) != "" {
			return text, nil
		}
		if time.Now().After(deadline) {
			var html string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
			return "", fmt.Errorf("timed out waiting for %s: %s", id, html)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var ready bool
			if err := chromedp.Evaluate(`document.documentElement.dataset.spikeReady === "1"`, &ready).Do(ctx); err != nil {
				return err
			}
			if ready {
				return nil
			}
			if time.Now().After(deadline) {
				return context.DeadlineExceeded
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
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
