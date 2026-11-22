package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/egress"
)

// appSettings is the part of Beacon.app's settings (~/Library/Application Support/beacon/app/settings.json) that
// names a host the app contacts. It holds no secret: the app keeps tokens in its vault.
type appSettings struct {
	GitHub *struct {
		Origin string `json:"origin"`
	} `json:"github"`
	Signal *struct {
		Origin string `json:"origin"`
	} `json:"signal"`
}

// appEgress lists the hosts Beacon.app contacts under its settings, so `beacon egress` shows every host either
// process may reach (AGENTS.md). No settings file means no app hosts.
func (a *app) appEgress() ([]egress.Host, error) {
	path := a.appSettingsFile
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "beacon", "app", "settings.json")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s appSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s is not Beacon.app's settings", path)
	}
	var hosts []egress.Host
	add := func(name, origin, purpose string) error {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return fmt.Errorf("%s: the %s origin is not https://host", path, name)
		}
		hosts = append(hosts, egress.Host{Address: egress.Address(u), Purpose: purpose})
		return nil
	}
	if s.GitHub != nil {
		if err := add("GitHub", s.GitHub.Origin,
			"Beacon.app: github.merge (reads before approval and before the merge, one merge per approval, read-back)"); err != nil {
			return nil, err
		}
	}
	if s.Signal != nil {
		if err := add("Signal", s.Signal.Origin, "Beacon.app: approval records, in journal order (POST /api/app-records)"); err != nil {
			return nil, err
		}
	}
	return hosts, nil
}
