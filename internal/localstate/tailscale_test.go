package localstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTailscaleCommandFindsMacAppCLI(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		pathCLI        bool
		systemMode     os.FileMode
		want           string
	}{
		{"PATH first", "darwin", true, 0o700, "path"},
		{"system app first", "darwin", false, 0o700, "system"},
		{"home app if system CLI is not executable", "darwin", false, 0o600, "home"},
		{"no app fallback on Linux", "linux", false, 0o700, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "Test Home")
			bin := filepath.Join(root, "bin")
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin)
			paths := map[string]string{
				"path":   filepath.Join(bin, "tailscale"),
				"system": filepath.Join(root, "Applications/Tailscale.app/Contents/MacOS/Tailscale"),
				"home":   filepath.Join(home, "Applications/Tailscale.app/Contents/MacOS/Tailscale"),
			}
			writeCLI := func(path string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\"\n"), mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pathCLI {
				writeCLI(paths["path"], 0o700)
			}
			writeCLI(paths["system"], tc.systemMode)
			writeCLI(paths["home"], 0o700)
			out, err := tailscaleCommand(t.Context(), tc.platform, paths["system"], "status", "--json").Output()
			if tc.want == "" {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("expected PATH lookup failure, got %v", err)
				}
			} else if want := paths[tc.want] + "\nstatus\n--json\n"; err != nil || string(out) != want {
				t.Fatalf("got %q, %v; want %q", out, err, want)
			}
		})
	}
}

func TestConfigurePrivateServeAndCheckReadBack(t *testing.T) {
	const correct = `{"TCP":{"443":{"HTTPS":true}},"Web":{"machine.tail.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7317"}}}}}`
	shared := strings.Replace(correct, `"/":`, `"/other":{"Proxy":"http://127.0.0.1:8989"},"/":`, 1)
	const status = `{"BackendState":"Running","Self":{"DNSName":"machine.tail.ts.net.","UserID":9},"User":{"9":{"LoginName":"person@example.test"}}}`
	for _, tc := range []struct {
		name, initial, after, refusal string
		wantRuns                      bool
	}{
		{"reuses", correct, correct, "", false},
		{"installs", `{}`, correct, "", true},
		{"does not overwrite", strings.ReplaceAll(correct, "7317", "1234"), correct, "does not proxy", false},
		{"checks reply", `{}`, `{}`, "is not running", true},
		{"rejects shared origin", shared, correct, "route exclusively", false},
		{"rejects shared origin after setup", `{}`, shared, "route exclusively", true},
		{"rejects foreground route", strings.TrimSuffix(correct, "}") + `,"Foreground":{"session":{"Web":{"machine.tail.ts.net:443":{"Handlers":{"/other":{"Text":"fixture"}}}}}}}`, correct, "foreground", false},
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
