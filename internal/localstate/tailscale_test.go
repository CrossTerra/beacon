package localstate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestConfigurePrivateServeAndCheckReadBack(t *testing.T) {
	const correct = `{"TCP":{"443":{"HTTPS":true}},"Web":{"machine.tail.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7317"}}}}}`
	const status = `{"BackendState":"Running","Self":{"DNSName":"machine.tail.ts.net.","UserID":9},"User":{"9":{"LoginName":"person@example.test"}}}`
	for _, tc := range []struct {
		name, initial, after, refusal string
		wantRuns                      bool
	}{
		{"reuses", correct, correct, "", false},
		{"installs", `{}`, correct, "", true},
		{"does not overwrite", strings.ReplaceAll(correct, "7317", "1234"), correct, "does not proxy", false},
		{"checks reply", `{}`, `{}`, "is not running", true},
		{"rejects funnel", strings.Replace(correct, `{"TCP"`, `{"AllowFunnel":{"machine.tail.ts.net:443":true},"TCP"`, 1), correct, "Funnel", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serve := tc.initial
			ran, agreed := false, false
			run := func(_ context.Context, args ...string) ([]byte, error) {
				switch strings.Join(args, " ") {
				case "status --json":
					return []byte(status), nil
				case "serve status --json":
					return []byte(serve), nil
				case "serve --bg --https=443 http://127.0.0.1:7317":
					if !agreed {
						t.Error("serve changed without confirmation")
					}
					ran, serve = true, tc.after
					return nil, nil
				}
				return nil, errors.New("unexpected command")
			}
			o, err := TailOrigin(t.Context(), run)
			if err != nil || o.URL() != "https://machine.tail.ts.net" || o.TailscaleLogin() != "person@example.test" {
				t.Fatalf("unexpected origin: %v %v", o, err)
			}
			err = EnsureServe(t.Context(), o, run, func(string) error { agreed = true; return nil })
			if got := err != nil; got != (tc.refusal != "") || got && !strings.Contains(err.Error(), tc.refusal) || ran != tc.wantRuns {
				t.Fatalf("configured: %v, %v", ran, err)
			}
		})
	}
	runTagged := func(_ context.Context, args ...string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"status", "--json"}) {
			t.Fatal(args)
		}
		return []byte(strings.Replace(status, `"UserID":9`, `"UserID":9,"Tags":["tag:fixture"]`, 1)), nil
	}
	if _, err := TailOrigin(t.Context(), runTagged); err == nil {
		t.Fatal("tagged machine accepted")
	}
}
