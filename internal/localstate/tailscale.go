package localstate

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const proxy = "http://127.0.0.1:" + approval.ReviewPort

type TailCommand func(context.Context, ...string) ([]byte, error)

func Tailscale(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "tailscale", args...).Output()
}

func TailOrigin(ctx context.Context, run TailCommand) (approval.Origin, error) {
	if run == nil {
		run = Tailscale
	}
	b, err := run(ctx, "status", "--json")
	if err != nil {
		return approval.Origin{}, errors.New("could not read Tailscale status; install and connect Tailscale first")
	}
	var s struct {
		BackendState string
		Self         struct {
			DNSName string
			UserID  uint64
			Tags    []string
		}
		User map[string]struct{ LoginName string }
	}
	if json.Unmarshal(b, &s) != nil || s.BackendState != "Running" || s.Self.UserID == 0 || len(s.Self.Tags) != 0 {
		return approval.Origin{}, errors.New("Tailscale must be connected as a user, not a tagged machine")
	}
	return approval.ParseOrigin("https://"+strings.TrimSuffix(s.Self.DNSName, "."), s.User[strconv.FormatUint(s.Self.UserID, 10)].LoginName)
}

type serveStatus struct {
	TCP map[string]struct {
		HTTPS      bool
		TCPForward string
	}
	Web map[string]struct {
		Handlers map[string]struct{ Proxy, Path, Text string }
	}
	AllowFunnel map[string]bool
}

func readServe(ctx context.Context, run TailCommand) (serveStatus, error) {
	var s serveStatus
	b, err := run(ctx, "serve", "status", "--json")
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, errors.New("could not read Tailscale Serve configuration")
	}
	return s, nil
}

func (s serveStatus) ready(o approval.Origin) (bool, error) {
	host := o.Host() + ":443"
	if s.AllowFunnel[host] {
		return false, errors.New("Tailscale Funnel is on for this origin; disable it first")
	}
	for route, web := range s.Web {
		if s.AllowFunnel[route] {
			for _, h := range web.Handlers {
				if strings.TrimSuffix(h.Proxy, "/") == proxy {
					return false, errors.New("Tailscale Funnel exposes the Beacon listener; disable it first")
				}
			}
		}
	}
	head, bound := s.TCP["443"]
	web, mapped := s.Web[host]
	if !bound && !mapped {
		return false, nil
	}
	root, ok := web.Handlers["/"]
	if !head.HTTPS || head.TCPForward != "" || !ok || strings.TrimSuffix(root.Proxy, "/") != proxy || root.Path != "" || root.Text != "" {
		return false, errors.New("Tailscale Serve port 443 does not proxy this origin to Beacon; existing configuration was not changed")
	}
	return true, nil
}

// EnsureServe configures only an unused endpoint and confirms what Tailscale actually installed.
func EnsureServe(ctx context.Context, o approval.Origin, run TailCommand, confirm func(string) error) error {
	if run == nil {
		run = Tailscale
	}
	s, err := readServe(ctx, run)
	if err != nil {
		return err
	}
	ready, err := s.ready(o)
	if err != nil || ready {
		return err
	}
	if err := confirm("Run tailscale serve --bg --https=443 " + proxy + "?"); err != nil {
		return err
	}
	if _, err := run(ctx, "serve", "--bg", "--https=443", proxy); err != nil {
		return errors.New("tailscale serve failed; check HTTPS certificates and permission to configure Serve")
	}
	return CheckServe(ctx, o, run)
}

func CheckServe(ctx context.Context, o approval.Origin, run TailCommand) error {
	if !o.Secure() {
		return nil
	}
	if run == nil {
		run = Tailscale
	}
	s, err := readServe(ctx, run)
	if err != nil {
		return err
	}
	ready, err := s.ready(o)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("the Tailscale Serve proxy for Beacon is not running")
	}
	return nil
}
