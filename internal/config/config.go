// Package config reads and writes Beacon's one config file. Secrets are never in it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/CrossTerra/beacon/internal/source"
)

// Mode says whether `beacon run` sends.
type Mode string

const (
	// DryRun collects and decides, prints what would be sent, and contacts no Signal host.
	DryRun Mode = "dry-run"
	// Send appends to the outbox and sends while this instance holds the lease.
	Send Mode = "send"
)

// Defaults.
const (
	DefaultLeasePeriod  = 5 * time.Minute
	DefaultPollInterval = time.Minute
)

// Config is the parsed config file. Relative paths are resolved against the file's directory.
type Config struct {
	Signal  Signal   `yaml:"signal"`
	Person  Person   `yaml:"person"`
	Rules   string   `yaml:"rules"`
	State   string   `yaml:"state"`
	Mode    Mode     `yaml:"mode"`
	Lease   Lease    `yaml:"lease,omitempty"`
	Poll    Duration `yaml:"poll_interval,omitempty"`
	Sources []Source `yaml:"sources"`

	// Path is the file the config was read from.
	Path string `yaml:"-"`
}

// Signal names the Signal host and the keychain account holding its key.
type Signal struct {
	URL             string `yaml:"url"`
	KeychainAccount string `yaml:"keychain_account,omitempty"`
}

// Person is the one person this Beacon serves.
type Person struct {
	// Handle is the person's Signal actor, "@handle" or "act_…". Asks name it in `to` (DEC-27).
	Handle string `yaml:"handle"`
	// Identities maps a source name to the person's own identity there. `by` is set from it on own acts (DEC-27).
	Identities map[string]string `yaml:"identities,omitempty"`
}

// Lease configures the send lease (DEC-25).
type Lease struct {
	Period Duration `yaml:"period,omitempty"`
}

// Source enables one source. A credential is never a field here: `beacon source add` stores it in the keychain.
type Source struct {
	Kind string `yaml:"kind"`
	// Name is the source name items and rules use. It defaults to Kind.
	Name string `yaml:"name,omitempty"`
	// Path is a fixture file.
	Path string `yaml:"path,omitempty"`
	// Host is a GitHub API host, host or host:port. Empty means api.github.com.
	Host string `yaml:"host,omitempty"`
	// Repos are GitHub repositories, owner/name or owner/*.
	Repos []string `yaml:"repos,omitempty"`
	// Projects are Jira project keys. On a GitHub source they limit issue-key references (DEC-17).
	Projects []string `yaml:"projects,omitempty"`
	// Site is a Jira Cloud site label. The host is <site>.atlassian.net.
	Site string `yaml:"site,omitempty"`
}

// SourceName is the name items and rules use.
func (s Source) SourceName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Kind
}

// Duration is a time.Duration written as "5m".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

var (
	handlePattern   = regexp.MustCompile(`^(@[a-z0-9][a-z0-9_.-]*|act_[A-Za-z0-9_-]+)$`)
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	repoWildPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/\*$`)
	projectPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]+$`)
	sitePattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?$`)
)

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	return c, nil
}

// Parse decodes and validates a config. Unknown keys are errors.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if _, err := SignalURL(c.Signal.URL); err != nil {
		errs = append(errs, err)
	}
	if !handlePattern.MatchString(c.Person.Handle) {
		errs = append(errs, fmt.Errorf("person.handle %q is not @handle or act_…", c.Person.Handle))
	}
	for src, id := range c.Person.Identities {
		if !source.ValidName(src) {
			errs = append(errs, fmt.Errorf("person.identities: %q is not a source name", src))
		}
		if strings.HasPrefix(id, "mailto:") {
			errs = append(errs, fmt.Errorf("person.identities.%s: email identities are email:, not mailto: (DEC-27)", src))
		}
		if strings.HasPrefix(id, "signal:") || !strings.Contains(id, ":") || strings.ContainsAny(id, " \t") {
			errs = append(errs, fmt.Errorf("person.identities.%s: %q is not an outside identity", src, id))
		}
	}
	if c.Rules == "" {
		errs = append(errs, errors.New("rules: the path of the rules file is required"))
	}
	if c.State == "" {
		errs = append(errs, errors.New("state: the path of the state directory is required"))
	}
	switch c.Mode {
	case DryRun, Send:
	default:
		errs = append(errs, fmt.Errorf("mode %q is neither dry-run nor send", c.Mode))
	}
	if c.LeasePeriod() < time.Minute {
		errs = append(errs, errors.New("lease.period must be at least 1m"))
	}
	if c.PollInterval() <= 0 || c.PollInterval() > c.LeasePeriod()/2 {
		errs = append(errs, errors.New("poll_interval must be positive and at most half of lease.period, so the lease is renewed in time"))
	}
	names := map[string]int{}
	for i, s := range c.Sources {
		name := s.SourceName()
		if !source.ValidName(name) {
			errs = append(errs, fmt.Errorf("sources[%d]: name %q is invalid", i, name))
		} else if prev, ok := names[name]; ok {
			errs = append(errs, fmt.Errorf("sources[%d]: name %q is already used by sources[%d]", i, name, prev))
		} else {
			names[name] = i
		}
		switch s.Kind {
		case "fixture":
			if s.Path == "" {
				errs = append(errs, fmt.Errorf("sources[%d]: a fixture needs path", i))
			}
			if s.Host != "" || len(s.Repos) > 0 || len(s.Projects) > 0 || s.Site != "" {
				errs = append(errs, fmt.Errorf("sources[%d]: a fixture takes only kind, name and path", i))
			}
		case "github":
			errs = append(errs, s.validateGitHub(i)...)
		case "jira":
			errs = append(errs, s.validateJira(i)...)
		default:
			errs = append(errs, fmt.Errorf("sources[%d]: unknown kind %q", i, s.Kind))
		}
	}
	return errors.Join(errs...)
}

func (s Source) validateGitHub(i int) []error {
	var errs []error
	at := fmt.Sprintf("sources[%d]", i)
	if s.Path != "" || s.Site != "" {
		errs = append(errs, fmt.Errorf("%s: a github source takes no path or site", at))
	}
	if len(s.Repos) == 0 {
		errs = append(errs, fmt.Errorf("%s: a github source needs repos", at))
	}
	for _, r := range s.Repos {
		if !repoPattern.MatchString(r) && !repoWildPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("%s: repo %q is not owner/name or owner/*", at, r))
		}
	}
	if s.Host != "" && !hostPattern.MatchString(s.Host) {
		errs = append(errs, fmt.Errorf("%s: host %q is not a host", at, s.Host))
	}
	for _, p := range s.Projects {
		if !projectPattern.MatchString(p) {
			errs = append(errs, fmt.Errorf("%s: project %q is not a Jira project key", at, p))
		}
	}
	return errs
}

func (s Source) validateJira(i int) []error {
	var errs []error
	at := fmt.Sprintf("sources[%d]", i)
	if s.Path != "" || s.Host != "" || len(s.Repos) > 0 {
		errs = append(errs, fmt.Errorf("%s: a jira source takes no path, host or repos", at))
	}
	if !sitePattern.MatchString(s.Site) {
		errs = append(errs, fmt.Errorf("%s: site %q is not a site label", at, s.Site))
	}
	if len(s.Projects) == 0 {
		errs = append(errs, fmt.Errorf("%s: a jira source needs projects", at))
	}
	for _, p := range s.Projects {
		if !projectPattern.MatchString(p) {
			errs = append(errs, fmt.Errorf("%s: project %q is not a Jira project key", at, p))
		}
	}
	return errs
}

// SignalURL parses a Signal base URL. It must be https, or http to a loopback address.
func SignalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("signal.url %q is not a base URL", raw)
	}
	if strings.Trim(u.Path, "/") != "" {
		return nil, fmt.Errorf("signal.url %q must not have a path", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, fmt.Errorf("signal.url %q: http is allowed only to a loopback address", raw)
		}
	default:
		return nil, fmt.Errorf("signal.url %q must be https", raw)
	}
	u.Path = ""
	return u, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SignalHost returns host:port of the Signal URL, with the scheme's default port filled in.
func (c *Config) SignalHost() string {
	u, err := SignalURL(c.Signal.URL)
	if err != nil {
		return ""
	}
	return HostPort(u)
}

// HostPort returns u's host with an explicit port.
func HostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "443"
	if u.Scheme == "http" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// KeychainAccount is the keychain account holding the Signal key.
func (c *Config) KeychainAccount() string {
	if c.Signal.KeychainAccount != "" {
		return c.Signal.KeychainAccount
	}
	return "signal:" + c.SignalHost()
}

// LeasePeriod is the lease period, defaulted.
func (c *Config) LeasePeriod() time.Duration {
	if c.Lease.Period == 0 {
		return DefaultLeasePeriod
	}
	return time.Duration(c.Lease.Period)
}

// PollInterval is how often `beacon run` collects, defaulted.
func (c *Config) PollInterval() time.Duration {
	if c.Poll == 0 {
		return DefaultPollInterval
	}
	return time.Duration(c.Poll)
}

// Resolve returns p relative to the config file's directory, unless it is absolute.
func (c *Config) Resolve(p string) string {
	if filepath.IsAbs(p) || c.Path == "" {
		return p
	}
	return filepath.Join(filepath.Dir(c.Path), p)
}

// RulesPath is the rules file.
func (c *Config) RulesPath() string { return c.Resolve(c.Rules) }

// StatePath is the state directory: the outbox, the hold queue and the decisions log.
func (c *Config) StatePath() string { return c.Resolve(c.State) }

// Marshal writes the config as YAML.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// DefaultPath is where the config lives unless --config or BEACON_CONFIG says otherwise.
func DefaultPath() (string, error) {
	if p := os.Getenv("BEACON_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beacon", "config.yaml"), nil
}
