// Package service handles the collector's last-pass record and reads the retired LaunchAgent's status and logs.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// LeafName is the certificate leaf hash recorded at install, in the state directory.
	LeafName = "service.leaf"

	// BackoffCap is the longest a poll loop waits before a non-zero exit on a persistent failure.
	BackoffCap = 30 * time.Minute

	// LogMaxBytes is the size at which a service log is rotated.
	LogMaxBytes = 1 << 20
	// rotateReadMax is the largest log that is copied aside. A larger file is truncated without a copy, so a
	// pathological file is not read into memory.
	rotateReadMax = 8 << 20
)

// AgentsDir is ~/Library/LaunchAgents.
func AgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// CollectorLog is ~/Library/Logs/Beacon/collector.log. tether appends the supervised collector's standard output,
// its pass summaries, there.
func CollectorLog() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "Beacon", "collector.log"), nil
}

// Domain is the GUI launchd domain for uid. A LaunchAgent there runs as that user, in the graphical session, and
// can read the login keychain. It is not the system domain.
func Domain(uid int) string { return "gui/" + strconv.Itoa(uid) }

// Target is the service target in that domain.
func Target(uid int) string { return Domain(uid) + "/" + Label }

// Rotate copies path aside to path.1 and truncates it when it is at least limit bytes.
//
// tether (and launchd before it) holds the log descriptor open and writes with append. A rename would leave that descriptor on the old
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
	// LeaseUntil is the held lease's local deadline. Older records did not store it.
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	LeaseReason string     `json:"lease_reason,omitempty"`
	SendReason  string     `json:"send_reason,omitempty"`
	// Failures is the streak of failed starts. It sets the backoff. A clean pass leaves it zero.
	Failures int `json:"failures,omitempty"`
	// Withheld are personal sources whose send items the pass did not send, because Signal does not restrict their
	// subject prefix or the restrictions could not be read.
	Withheld []Withheld `json:"withheld,omitempty"`
}

// Withheld is one personal source a pass did not send for. Every field is a name or a fixed code, never content.
type Withheld struct {
	Source string `json:"source"`
	Prefix string `json:"prefix"`
	Reason string `json:"reason"`
}

// Reasons a pass withholds a personal source.
const (
	WithheldUnrestricted = "prefix_unrestricted"
	WithheldUnknown      = "restrictions_unknown"
)

// Lease values recorded on a pass.
const (
	LeaseHeld     = "held"
	LeaseOther    = "other"
	LeaseNotTaken = "not_taken"
)

var (
	outcomePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	holderPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	sourcePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	prefixPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}:\*$`)
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
	if len(p.Withheld) > 16 {
		return ErrPass
	}
	for _, w := range p.Withheld {
		if !sourcePattern.MatchString(w.Source) || !prefixPattern.MatchString(w.Prefix) || (w.Reason != WithheldUnrestricted && w.Reason != WithheldUnknown) {
			return ErrPass
		}
	}
	if p.Lease != LeaseHeld && p.Holder != "" {
		return ErrPass
	}
	if p.LeaseUntil != nil && (p.Lease != LeaseHeld || p.LeaseUntil.IsZero()) {
		return ErrPass
	}
	for _, reason := range []string{p.LeaseReason, p.SendReason} {
		if reason != "" && !outcomePattern.MatchString(reason) {
			return ErrPass
		}
	}
	if p.Failures < 0 {
		return ErrPass
	}
	return nil
}

// Failed reports whether outcome is a recorded failure, not a clean pass or a stop.
func Failed(outcome string) bool {
	return outcome != "" && outcome != "ok" && outcome != "stopped" && outcomePattern.MatchString(outcome)
}

// NextFailures is the streak after one more failure. A clean previous pass starts it at 1.
func NextFailures(prev Pass, ok bool) int {
	if !ok || !Failed(prev.Outcome) {
		return 1
	}
	n := prev.Failures
	if n < 1 {
		n = 1
	}
	if n >= 1000 {
		return 1000
	}
	return n + 1
}

// Backoff is how long to wait before a non-zero exit: one poll interval, doubling, capped at BackoffCap.
func Backoff(poll time.Duration, failures int) time.Duration {
	if poll <= 0 {
		poll = time.Minute
	}
	if failures < 1 {
		failures = 1
	}
	d := poll
	for i := 1; i < failures && d < BackoffCap; i++ {
		if d > BackoffCap/2 {
			return BackoffCap
		}
		d *= 2
	}
	if d > BackoffCap {
		return BackoffCap
	}
	return d
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
	// Leaf is the SHA-256 of the signing certificate, hex. It is what install pins. A rebuild with the same
	// certificate keeps it; a different signer does not.
	Leaf string
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
	leaf, err := leafHash(ctx, path)
	if err != nil {
		return Identity{}, ErrNotStable
	}
	id.Leaf = leaf
	if !id.Stable() || !LeafOK(id.Leaf) {
		return Identity{}, ErrNotStable
	}
	return id, nil
}

// HashLeaf is the SHA-256 of a leaf certificate, hex. Install records it; run and status compare it.
func HashLeaf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// LeafOK reports whether s is a leaf hash this package would write.
func LeafOK(s string) bool { return leafPattern.MatchString(s) }

var leafPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func leafHash(ctx context.Context, path string) (string, error) {
	dir, err := os.MkdirTemp("", "beacon-codesign")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "cert")
	if err := exec.CommandContext(ctx, "codesign", "-d", "--extract-certificates="+prefix, path).Run(); err != nil {
		return "", ErrNotStable
	}
	der, err := os.ReadFile(prefix + "0")
	if err != nil || len(der) == 0 {
		return "", ErrNotStable
	}
	return HashLeaf(der), nil
}

// WriteLeaf records the leaf hash, mode 0600.
func WriteLeaf(dir, leaf string) error {
	if !LeafOK(leaf) {
		return errors.New("service: signing identity is unreadable")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(dir, LeafName, []byte(leaf+"\n"))
}

// ReadLeaf reads the recorded leaf hash. A missing file is os.ErrNotExist. Anything else that is not a hash is a
// fixed error and does not include the file.
func ReadLeaf(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, LeafName))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if !LeafOK(s) {
		return "", ErrIdentity
	}
	return s, nil
}

// ErrIdentity means the recorded signing identity is not a leaf hash. Its text does not include the file.
var ErrIdentity = errors.New("service: signing identity is unreadable")

// BinaryFromPlist returns the executable named by a plist this package wrote, or "" if it cannot.
func BinaryFromPlist(data []byte) string {
	const key = "<key>ProgramArguments</key>"
	i := bytes.Index(data, []byte(key))
	if i < 0 {
		return ""
	}
	rest := data[i+len(key):]
	start := bytes.Index(rest, []byte("<string>"))
	if start < 0 {
		return ""
	}
	rest = rest[start+len("<string>"):]
	end := bytes.Index(rest, []byte("</string>"))
	if end < 0 {
		return ""
	}
	s := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(string(rest[:end]))
	if !filepath.IsAbs(s) || strings.ContainsAny(s, "\r\n") {
		return ""
	}
	return s
}

// FormatStatus is the fixed status text. Fields are already validated; this does not read a file.
func FormatStatus(job Job, pass Pass, passErr error, identity string) string {
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
	switch identity {
	case "ok", "mismatch", "none", "unreadable":
	default:
		identity = "unreadable"
	}
	head, withheld := passLines(pass, passErr)
	return fmt.Sprintf("loaded: %s\nrunning: %s\npid: %s\n", loaded, running, pid) + head + "identity: " + identity + "\n" + withheld
}

// FormatPass is the last pass as `beacon status` prints it: the pass.json lines of FormatStatus, without the job.
func FormatPass(pass Pass, passErr error) string {
	head, withheld := passLines(pass, passErr)
	return head + withheld
}

// passLines are the last pass's time, outcome, lease and holder, and a line per personal source it withheld.
func passLines(pass Pass, passErr error) (string, string) {
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
	head := fmt.Sprintf("last_pass: %s\noutcome: %s\nlease: %s\nholder: %s\n", last, outcome, lease, holder)
	withheld := ""
	if passErr == nil {
		// One line per personal source the last pass did not send for, and why: ask Signal's admin to restrict the
		// prefix, or give Beacon's key the read scope.
		for _, w := range pass.Withheld {
			withheld += fmt.Sprintf("withheld: %s %s %s\n", w.Source, w.Prefix, w.Reason)
		}
	}
	return head, withheld
}
