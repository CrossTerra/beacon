// Package service is the per-user launchd agent: the job definition, the last-pass record, and log rotation.
//
// It does not read a credential, and it does not send. The plist it writes names a binary, a config path and two log
// paths. It has no environment variables, so it cannot carry a secret. Keys stay in the keychain.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Label is the launchd job label.
	Label = "dev.beacon.agent"
	// PlistName is the file in ~/Library/LaunchAgents.
	PlistName = "dev.beacon.agent.plist"
	// StdoutName and StderrName are the log files in the state directory.
	StdoutName = "service.stdout.log"
	StderrName = "service.stderr.log"
	// PassName is the last send pass, in the state directory.
	PassName = "pass.json"

	// ThrottleInterval is how long launchd waits before restarting a job that exited too quickly. It is launchd's
	// own default, written into the plist so the bound is visible.
	ThrottleInterval = 10
	// ExitTimeOut is how long launchd waits after SIGTERM before SIGKILL. It covers one send, including one
	// Retry-After of up to two minutes. It is not infinity: a send still waiting after this can be killed, and the
	// outbox recovers that the same way it recovers a crash.
	ExitTimeOut = 180
	// LogMaxBytes is the size at which a service log is rotated.
	LogMaxBytes = 1 << 20
	// rotateReadMax is the largest log that is copied aside. A larger file is truncated without a copy, so a
	// pathological file is not read into memory.
	rotateReadMax = 8 << 20

	// umaskDecimal is 077. Property lists have no octal integers.
	umaskDecimal = 63
)

// AgentsDir is ~/Library/LaunchAgents.
func AgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// Domain is the GUI launchd domain for uid. A LaunchAgent there runs as that user, in the graphical session, and
// can read the login keychain. It is not the system domain.
func Domain(uid int) string { return "gui/" + strconv.Itoa(uid) }

// Target is the service target in that domain.
func Target(uid int) string { return Domain(uid) + "/" + Label }

// Spec is everything the plist names. Paths are absolute. Nothing in it is a credential.
type Spec struct {
	Binary string
	Config string
	Stdout string
	Stderr string
}

// Plist returns the launchd job definition. The bytes are stable for a given spec.
func Plist(spec Spec) ([]byte, error) {
	binary, err := xmlText(spec.Binary)
	if err != nil {
		return nil, err
	}
	config, err := xmlText(spec.Config)
	if err != nil {
		return nil, err
	}
	stdout, err := xmlText(spec.Stdout)
	if err != nil {
		return nil, err
	}
	stderr, err := xmlText(spec.Stderr)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{spec.Binary, spec.Config, spec.Stdout, spec.Stderr} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("service: paths must be absolute")
		}
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	writeKey(&b, "Label", "<string>"+Label+"</string>")
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	b.WriteString("\t\t<string>" + binary + "</string>\n")
	b.WriteString("\t\t<string>run</string>\n")
	b.WriteString("\t\t<string>--config</string>\n")
	b.WriteString("\t\t<string>" + config + "</string>\n")
	b.WriteString("\t</array>\n")
	writeKey(&b, "RunAtLoad", "<true/>")
	b.WriteString("\t<key>KeepAlive</key>\n")
	b.WriteString("\t<dict>\n")
	b.WriteString("\t\t<key>SuccessfulExit</key>\n")
	b.WriteString("\t\t<false/>\n")
	b.WriteString("\t</dict>\n")
	writeKey(&b, "ThrottleInterval", "<integer>"+strconv.Itoa(ThrottleInterval)+"</integer>")
	writeKey(&b, "ExitTimeOut", "<integer>"+strconv.Itoa(ExitTimeOut)+"</integer>")
	writeKey(&b, "Umask", "<integer>"+strconv.Itoa(umaskDecimal)+"</integer>")
	writeKey(&b, "ProcessType", "<string>Background</string>")
	writeKey(&b, "StandardOutPath", "<string>"+stdout+"</string>")
	writeKey(&b, "StandardErrorPath", "<string>"+stderr+"</string>")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return []byte(b.String()), nil
}

func writeKey(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>" + key + "</key>\n")
	b.WriteString("\t" + value + "\n")
}

func xmlText(s string) (string, error) {
	if s == "" {
		return "", errors.New("service: empty path")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("service: path contains a control character")
		}
	}
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Rotate copies path aside to path.1 and truncates it when it is at least limit bytes.
//
// launchd holds the log descriptor open and writes with append. A rename would leave that descriptor on the old
// inode, so the file is truncated in place. The copy happens first. A file larger than rotateReadMax is truncated
// without a copy.
func Rotate(path string, limit int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() < limit) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= rotateReadMax {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := writeFile(path+".1", data); err != nil {
			return err
		}
	}
	return os.Truncate(path, 0)
}

func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Pass is the last send pass, as status reports it. It holds no item content.
type Pass struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Lease   string    `json:"lease,omitempty"`
	Holder  string    `json:"holder,omitempty"`
}

// Lease values recorded on a pass.
const (
	LeaseHeld     = "held"
	LeaseOther    = "other"
	LeaseNotTaken = "not_taken"
)

var (
	outcomePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	holderPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ErrPass means the pass record is missing a required field or is not in the fixed set. Its text is fixed: it does
// not include the file.
var ErrPass = errors.New("service: pass record is unreadable")

// WritePass writes the pass record atomically, mode 0600, synced.
func WritePass(dir string, p Pass) error {
	if err := validate(p); err != nil {
		return err
	}
	p.At = p.At.UTC()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(dir, PassName, data)
}

// ReadPass reads the pass record. A missing file is os.ErrNotExist. Any other problem is ErrPass, which carries
// none of the file.
func ReadPass(dir string) (Pass, error) {
	f, err := os.Open(filepath.Join(dir, PassName))
	if err != nil {
		return Pass{}, err
	}
	defer f.Close()
	var p Pass
	dec := json.NewDecoder(io.LimitReader(f, 1<<12))
	if err := dec.Decode(&p); err != nil {
		return Pass{}, ErrPass
	}
	if err := validate(p); err != nil {
		return Pass{}, ErrPass
	}
	p.At = p.At.UTC()
	return p, nil
}

func validate(p Pass) error {
	if p.At.IsZero() || !outcomePattern.MatchString(p.Outcome) {
		return ErrPass
	}
	switch p.Lease {
	case "", LeaseHeld, LeaseOther, LeaseNotTaken:
	default:
		return ErrPass
	}
	if p.Holder != "" && !holderPattern.MatchString(p.Holder) {
		return ErrPass
	}
	if p.Lease == LeaseHeld && p.Holder == "" {
		return ErrPass
	}
	if p.Lease != LeaseHeld && p.Holder != "" {
		return ErrPass
	}
	return nil
}

func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Job is what `launchctl print` said about the agent. It is empty when the job is not loaded.
type Job struct {
	Loaded  bool
	Running bool
	State   string
	PID     int
	Program string
}

// ParsePrint reads `launchctl print` output. A job with a state line is loaded. Only state "running" is running.
func ParsePrint(out string) Job {
	var job Job
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "state = "):
			job.Loaded = true
			job.State = strings.TrimPrefix(line, "state = ")
			job.Running = job.State == "running"
		case strings.HasPrefix(line, "pid = "):
			n, err := strconv.Atoi(strings.TrimPrefix(line, "pid = "))
			if err == nil && n > 0 {
				job.PID = n
			}
		case strings.HasPrefix(line, "program = "):
			job.Program = strings.TrimPrefix(line, "program = ")
		}
	}
	return job
}

// Identity is what codesign reported about a binary. It is not a credential.
type Identity struct {
	Identifier string
	Authority  string
	AdHoc      bool
}

// Stable reports whether the identity is a certificate, not an ad-hoc signature. An ad-hoc signature changes every
// rebuild, so the keychain treats each rebuild as a different program.
func (id Identity) Stable() bool {
	return id.Authority != "" && !id.AdHoc
}

// ParseIdentity reads `codesign -dvv` output.
func ParseIdentity(out string) Identity {
	var id Identity
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Identifier="):
			id.Identifier = strings.TrimPrefix(line, "Identifier=")
		case strings.HasPrefix(line, "Authority=") && id.Authority == "":
			id.Authority = strings.TrimPrefix(line, "Authority=")
		case line == "Signature=adhoc" || strings.Contains(line, "(adhoc)"):
			id.AdHoc = true
		}
	}
	return id
}

// ErrNotStable means the binary is unsigned, ad-hoc signed, or failed verification.
var ErrNotStable = errors.New("service: binary is not code-signed with a stable identity")

// CodesignIdentity runs codesign. The error is ErrNotStable and does not include codesign's text.
func CodesignIdentity(ctx context.Context, path string) (Identity, error) {
	if _, err := exec.CommandContext(ctx, "codesign", "--verify", "--strict", path).CombinedOutput(); err != nil {
		return Identity{}, ErrNotStable
	}
	out, err := exec.CommandContext(ctx, "codesign", "-dvv", path).CombinedOutput()
	if err != nil {
		return Identity{}, ErrNotStable
	}
	id := ParseIdentity(string(out))
	if !id.Stable() {
		return Identity{}, ErrNotStable
	}
	return id, nil
}

// EnsureLog creates path if needed, mode 0600, without truncating an existing file.
func EnsureLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cerr := f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return cerr
}

// FormatStatus is the fixed status text. Fields are already validated; this does not read a file.
func FormatStatus(job Job, pass Pass, passErr error) string {
	loaded, running, pid := "no", "no", "none"
	if job.Loaded {
		loaded = "yes"
	}
	if job.Running {
		running = "yes"
		if job.PID > 0 {
			pid = strconv.Itoa(job.PID)
		}
	}
	last, outcome, lease, holder := "none", "none", "none", "none"
	switch {
	case passErr == nil && !pass.At.IsZero():
		last = pass.At.UTC().Format(time.RFC3339)
		outcome = pass.Outcome
		if pass.Lease != "" {
			lease = pass.Lease
		}
		switch {
		case pass.Holder != "":
			holder = pass.Holder
		case pass.Lease == LeaseOther:
			holder = "another instance"
		}
	case passErr != nil && !errors.Is(passErr, os.ErrNotExist):
		last, outcome = "unreadable", "unreadable"
	}
	return fmt.Sprintf("loaded: %s\nrunning: %s\npid: %s\nlast_pass: %s\noutcome: %s\nlease: %s\nholder: %s\n",
		loaded, running, pid, last, outcome, lease, holder)
}
