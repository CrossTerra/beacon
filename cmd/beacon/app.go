package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/seal"
	"github.com/CrossTerra/beacon/internal/seed"
	"github.com/CrossTerra/beacon/internal/service"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/slack"
	"github.com/CrossTerra/beacon/internal/source"
	"github.com/CrossTerra/beacon/internal/titled"
)

// Set by the linker: -X main.version=… -X main.commit=….
var (
	version = "dev"
	commit  = ""
)

// app is the CLI with its outside world passed in, so tests run it in-process.
type app struct {
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	keychain  func() (keychain.Store, error)
	transport func(allowed []string) http.RoundTripper
	now       func() time.Time

	// configureClient, when set, adjusts the Signal client (tests shorten its backoff).
	configureClient func(*signal.Client)
	// afterAppend is passed to the runner (tests stop the process there).
	afterAppend func(key string)
	// ghToken reads a GitHub token. Nil runs `gh auth token`. Tests set it so the token is never printed.
	ghToken func(context.Context, string) (string, error)

	// interrupt cancels an in-flight send. Nil means the context passed to main does. main sets it to SIGINT only,
	// so SIGTERM can finish the current send.
	interrupt context.Context
	// term is closed on SIGTERM. Nil in tests unless a test is simulating it.
	term context.Context
	// euid, uid, agentsDir, launchctl and identify are set by tests. Nil uses the process and, on macOS, launchd.
	euid      func() int
	uid       func() int
	agentsDir func() (string, error)
	launchctl launcher
	identify  func(context.Context, string) (service.Identity, error)
	// sleep waits: the service's backoff, and a Slack source's spacing between calls. Nil sleeps on the clock. Tests
	// record the duration and return.
	sleep func(context.Context, time.Duration) error
	// google replaces Google's endpoints and the browser opener. Empty uses Google's. Tests set it.
	google googleEndpoints
	// slackBase replaces https://slack.com. Tests set it.
	slackBase string
}

type googleEndpoints struct {
	tokenURL, authURL, gmailBase, apiBase string
	open                                  func(string) error
}

const usage = `usage: beacon <command> [flags]

Commands:
  init          write a config and an empty rules file, and store the Signal key in the keychain
  key set       replace the Signal key in the keychain, after Signal accepts it
  run           collect, apply rules and projection; print what would be sent (dry run), or send
  explain KEY   show the matching rule, the classifier's answer and the exact body for one item
  outbox        list what was sent, with keys, times and Signal event ids
  outbox retry  re-project rejected events Signal did not store, and send them (--rejected)
  cursor reset  clear a source cursor so the next collect reads open state
  egress        print every host Beacon may contact under the current config
  rules check   validate the rules file and report rules that can never match
  source add    store a source credential in the keychain and enable the source (github, jira, google, slack)
  seed          import a seed file of subjects already decided never or send
  accept-first  allow sending for sources whose first run has been reviewed
  service       install, uninstall, status or logs of the per-user launchd agent
  keychain repair  re-create each Beacon keychain item with this build's access, reading each value once
  version       print the version, commit and build flags

Every command but version and keychain repair takes --config PATH (default: $BEACON_CONFIG, else the user config
directory).
`

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func (a *app) main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "init":
		err = a.cmdInit(ctx, rest)
	case "key":
		err = a.cmdKey(ctx, rest)
	case "run":
		err = a.cmdRun(ctx, rest)
	case "explain":
		err = a.cmdExplain(ctx, rest)
	case "outbox":
		if len(rest) > 0 && rest[0] == "retry" {
			err = a.cmdOutboxRetry(ctx, rest[1:])
			break
		}
		err = a.cmdOutbox(rest)
	case "cursor":
		err = a.cmdCursor(rest)
	case "egress":
		err = a.cmdEgress(rest)
	case "rules":
		if len(rest) == 0 || rest[0] != "check" {
			fmt.Fprint(a.stderr, "usage: beacon rules check [--config PATH]\n")
			return 2
		}
		err = a.cmdRulesCheck(rest[1:])
	case "source":
		err = a.cmdSource(ctx, rest)
	case "seed":
		err = a.cmdSeed(rest)
	case "accept-first":
		err = a.cmdAcceptFirst(rest)
	case "service":
		err = a.cmdService(ctx, rest)
	case "keychain":
		err = a.cmdKeychain(rest)
	case "version", "--version":
		err = a.cmdVersion()
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, usage)
		return 0
	default:
		fmt.Fprintf(a.stderr, "beacon: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, flag.ErrHelp):
		return 2
	default:
		fmt.Fprintf(a.stderr, "beacon: %v\n", err)
		return 1
	}
}

func (a *app) flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	path := fs.String("config", "", "config file")
	return fs, path
}

func configPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return config.DefaultPath()
}

func (a *app) loadConfig(p string) (*config.Config, error) {
	path, err := configPath(p)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// env is a loaded config with its sources and rules.
type env struct {
	cfg     *config.Config
	sources []source.Source
	rules   *rules.File
	person  project.Person
}

func (a *app) load(p string) (*env, error) {
	cfg, err := a.loadConfig(p)
	if err != nil {
		return nil, err
	}
	e := &env{cfg: cfg, person: project.Person{Handle: cfg.Person.Handle, Identities: cfg.Person.Identities}}
	e.sources, err = a.buildSources(cfg)
	if err != nil {
		return nil, err
	}
	if a.transport != nil {
		u, _ := config.SignalURL(cfg.Signal.URL)
		client := limited.HTTPClient(a.transport(egress.Addresses(egress.List(u, e.sources))))
		for _, s := range e.sources {
			if h, ok := s.(interface{ SetHTTP(*http.Client) }); ok {
				h.SetHTTP(client)
			}
		}
	}
	e.rules, err = rules.Load(cfg.RulesPath())
	if err != nil {
		return nil, err
	}
	// A source that can skip reading bodies, or names, is told whether any rule could send them.
	for _, s := range e.sources {
		if b, ok := s.(interface{ SetBodies(bool) }); ok {
			// Slack reads message text with the message anyway, and a public channel's body may leave plain. A Google
			// source reads a body only for a sealed rule, since a personal body leaves only sealed (DEC-32).
			if _, isSlack := s.(*slack.Source); isSlack {
				b.SetBodies(e.rules.Lists(s.Name(), project.FieldBody))
			} else {
				b.SetBodies(e.rules.ListsSealed(s.Name(), project.FieldBody))
			}
		}
		if n, ok := s.(interface{ SetNames(bool) }); ok {
			n.SetNames(e.rules.Lists(s.Name(), slack.FieldTitle) || e.rules.Lists(s.Name(), slack.FieldChannelName) ||
				e.rules.Lists(s.Name(), slack.FieldAuthorName))
		}
	}
	return e, nil
}

func (e *env) checkSources() map[string]rules.Source {
	out := map[string]rules.Source{}
	for _, s := range e.sources {
		rs := rules.Source{Fields: s.Fields(), StateFields: s.StateFields(), Identity: e.cfg.Person.Identities[s.Name()] != ""}
		if _, ok := s.(interface{ PersonalStem() string }); ok {
			rs.Personal = rules.PersonalAll
		}
		if _, ok := s.(*slack.Source); ok {
			rs.Personal, rs.PersonalField, rs.PersonalValues = rules.PersonalSome, "channel_type", slack.PersonalKinds
		}
		out[s.Name()] = rs
	}
	return out
}

func (e *env) egress() []egress.Host {
	u, _ := config.SignalURL(e.cfg.Signal.URL)
	return egress.List(u, e.sources)
}

// --- init

const emptyRules = `# Beacon rules. First match wins; an item no rule matches is never sent.
# Grammar: docs/rules.md. Check with: beacon rules check
rules: []
`

func (a *app) cmdInit(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("init")
	url := fs.String("signal-url", "", "Signal base URL, https (or http to a loopback address)")
	handle := fs.String("handle", "", "your Signal handle, @handle or act_…")
	account := fs.String("keychain-account", "", "keychain account for the Signal key (default signal:<host:port>)")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init takes no arguments")
	}
	path, err := configPath(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists; pass --force to overwrite it", path)
	}
	cfg := &config.Config{
		Signal:  config.Signal{URL: strings.TrimRight(*url, "/"), KeychainAccount: *account},
		Person:  config.Person{Handle: *handle},
		Rules:   "rules.yaml",
		State:   "state",
		Mode:    config.DryRun,
		Sources: []config.Source{},
	}
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if _, err := config.Parse(data); err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stderr, "Signal key for %s (input hidden): ", cfg.Signal.URL)
	secret, err := keychain.ReadSecret(ctx, a.stdin)
	fmt.Fprintln(a.stderr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	cfg.Path, _ = filepath.Abs(path)
	if _, err := os.Stat(cfg.RulesPath()); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg.RulesPath(), []byte(emptyRules), 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.StatePath(), 0o700); err != nil {
		return err
	}
	if err := store.Set(cfg.KeychainAccount(), secret); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Wrote %s\nRules: %s\nState: %s\nSignal key stored in the keychain as %s/%s\nMode: dry-run. Set mode: send in the config to start sending.\n",
		path, cfg.RulesPath(), cfg.StatePath(), keychain.Service, cfg.KeychainAccount())
	return nil
}

// --- keychain repair

// cmdKeychain re-creates every Beacon item in the keychain, so each one carries the access a new item gets from this
// build and none of the partitions earlier builds added. It reads each value once and prints only account names.
func (a *app) cmdKeychain(args []string) error {
	if len(args) == 0 || args[0] != "repair" {
		return errors.New("usage: beacon keychain repair")
	}
	if len(args) > 1 {
		return errors.New("keychain repair takes no arguments")
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, "macOS asks once for each Beacon item. Choose Always Allow.")
	n := 0
	err = keychain.Repair(store, func(account string) {
		n++
		fmt.Fprintf(a.stdout, "Repaired %s/%s\n", keychain.Service, account)
	})
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(a.stdout, "No Beacon items in the keychain.")
	}
	return nil
}

// --- key set

func (a *app) cmdKey(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return fmt.Errorf("usage: beacon key set [--config PATH]")
	}
	fs, cfgPath := a.flags("key set")
	force := fs.Bool("force", false, "store the key even if its actor is not the configured person or their agent")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("key set takes no arguments")
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stderr, "Signal key for %s (input hidden): ", e.cfg.Signal.URL)
	secret, err := keychain.ReadSecret(ctx, a.stdin)
	fmt.Fprintln(a.stderr)
	if err != nil {
		return err
	}
	u, err := config.SignalURL(e.cfg.Signal.URL)
	if err != nil {
		return err
	}
	client := signal.New(u, a.transport(egress.Addresses(e.egress())), secret)
	if a.configureClient != nil {
		a.configureClient(client)
	}
	me, err := client.Me(ctx)
	if err != nil {
		if errors.Is(err, signal.ErrUnauthorized) {
			return errors.New("signal refused the key (401); it was not stored")
		}
		return errors.New("signal did not accept the key; it was not stored")
	}
	var old signal.Me
	if existing, err := store.Get(e.cfg.KeychainAccount()); err == nil {
		prev := signal.New(u, a.transport(egress.Addresses(e.egress())), existing)
		if a.configureClient != nil {
			a.configureClient(prev)
		}
		old, err = prev.Me(ctx)
		if err != nil && !errors.Is(err, signal.ErrUnauthorized) {
			return errors.New("the key already stored could not be checked; the new key was not stored")
		}
	}
	if !*force && !actorAllowed(me, old, e.cfg.Person.Handle) {
		return errors.New("the key's actor is not the configured person or their agent; pass --force to store it")
	}
	if err := store.Set(e.cfg.KeychainAccount(), secret); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Signal key replaced in the keychain as %s/%s\n", keychain.Service, e.cfg.KeychainAccount())
	if me.Handle != "" {
		h := me.Handle
		if !strings.HasPrefix(h, "@") && !strings.HasPrefix(h, "act_") {
			h = "@" + h
		}
		fmt.Fprintf(a.stdout, "Actor: %s\n", h)
	}
	if len(me.Scopes) > 0 {
		sort.Strings(me.Scopes)
		fmt.Fprintf(a.stdout, "Scopes: %s\n", strings.Join(me.Scopes, ", "))
	}
	return nil
}

// actorAllowed reports whether next is the configured person, the actor the stored key already is, or an agent of
// that person. A handle is compared without "@".
func actorAllowed(next, prev signal.Me, person string) bool {
	person = strings.TrimPrefix(strings.TrimSpace(person), "@")
	if person == "" || next.ID == "" {
		return false
	}
	if next.ID == person || strings.EqualFold(next.Handle, person) {
		return true
	}
	// The stored key was missing or refused, so it cannot name its actor. A replacement Beacon key is an
	// agent; a different person still needs --force.
	if prev.ID == "" {
		return next.Type == "agent"
	}
	if next.ID == prev.ID {
		return true
	}
	if prev.Handle != "" && next.Handle != "" && strings.EqualFold(next.Handle, prev.Handle) {
		return true
	}
	// works_for is an actor id, never a handle.
	if next.WorksFor != "" && (next.WorksFor == person || (prev.Type == "human" && next.WorksFor == prev.ID) || (prev.WorksFor != "" && next.WorksFor == prev.WorksFor)) {
		return true
	}
	return false
}

// --- run

func (a *app) cmdRun(ctx context.Context, args []string) error {
	if err := a.refuseRoot(); err != nil {
		return err
	}
	fs, cfgPath := a.flags("run")
	dry := fs.Bool("dry-run", false, "print what would be sent, and send nothing (the default until the config says mode: send)")
	first := fs.Bool("first", false, "dry run of open state, with a summary; sending stays off until beacon accept-first")
	once := fs.Bool("once", false, "make one pass and exit")
	bodies := fs.Bool("bodies", false, "print each decided item's body (a terminal dry run does; the launchd agent does not, unless this is set)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := logs.New(a.stderr)
	interrupt := ctx
	if a.interrupt != nil {
		interrupt = a.interrupt
	}
	// From here on, an error's text never reaches standard error: it can name keys, paths and rule patterns. The
	// person sees a fixed message for its reason, and the commands that show details. A poll loop records the reason
	// and waits before the non-zero exit, so launchd does not restart a persistent failure every throttle interval.
	var stateDir string
	poll := config.DefaultPollInterval
	// --once and --first are one-shot. Only the poll loop waits before a non-zero exit.
	fail := func(reason logs.Reason, text string) error {
		return a.failLoop(*once || *first, stateDir, poll, interrupt, log, reason, text, false)
	}
	// A rules or source error fails load, but the config is already readable. Take the state directory and rotate
	// before that failure, so status can show the reason and the service log stays capped.
	if cfg, err := a.loadConfig(*cfgPath); err == nil {
		stateDir = cfg.StatePath()
		poll = cfg.PollInterval()
		a.rotateServiceLogs(stateDir)
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return fail(logs.ReasonConfig, "the config, the rules file or a source could not be loaded; beacon rules check shows why")
	}
	if stateDir == "" {
		stateDir = e.cfg.StatePath()
		poll = e.cfg.PollInterval()
		a.rotateServiceLogs(stateDir)
	}
	if err := a.refusePinned(ctx, stateDir); err != nil {
		return fail(logs.ReasonSigning, "refusing to run a binary whose signing identity does not match the one recorded by beacon service install")
	}
	var problems int
	for _, p := range e.rules.Check(e.checkSources()) {
		if p.Error() {
			problems++
		}
	}
	if problems > 0 {
		return fail(logs.ReasonConfig, fmt.Sprintf("the rules file has %d errors; beacon rules check shows them", problems))
	}
	send := e.cfg.Mode == config.Send && !*dry && !*first
	// A dry run writes nothing to the state directory, a name cache included.
	for _, s := range e.sources {
		if p, ok := s.(interface{ SetPersist(bool) }); ok {
			p.SetPersist(send)
		}
	}
	r := &runner.Runner{Sources: e.sources, Rules: e.rules, Person: e.person, Send: send, Log: log, Now: a.now,
		AfterAppend: a.afterAppend, SealOwner: &e.cfg.Seal.Owner}
	// A dry run reads cursors and does not write them. No lock: it must not leave a resume a send pass trusts.
	if err := a.prepare(e, r, send); err != nil {
		return fail(runner.Classify(err), failText(runner.Classify(err), e))
	}
	// SIGTERM finishes the write already in progress and does not start another. SIGINT still cancels work.
	r.Stop = a.termFired
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	go func() {
		select {
		case <-interrupt.Done():
			cancelWork()
		case <-work.Done():
		}
	}()

	if !send {
		ob, err := outbox.Read(e.cfg.StatePath())
		if err != nil {
			return fail(runner.Classify(err), failText(runner.Classify(err), e))
		}
		q, err := queue.Read(e.cfg.StatePath())
		if err != nil {
			return fail(runner.Classify(err), failText(runner.Classify(err), e))
		}
		r.Outbox, r.Queue = ob, q
		for pass := 0; ; pass++ {
			if a.termFired() {
				log.Log(logs.RunStopped)
				return nil
			}
			a.rotateServiceLogs(stateDir)
			if pass > 0 {
				if err := a.prepare(e, r, send); err != nil {
					return fail(runner.Classify(err), failText(runner.Classify(err), e))
				}
			}
			rep, err := r.Run(work)
			if err != nil {
				return fail(runner.Classify(err), failText(runner.Classify(err), e))
			}
			a.printReport(rep, service.ReportBodies(*bodies, a.serviceStdout(stateDir), true))
			if *first {
				a.printFirst(rep)
				if err := a.recordFirst(e, rep); err != nil {
					return fail(runner.Classify(err), failText(runner.Classify(err), e))
				}
			}
			// --first is a summary, not a daemon. A dry run otherwise loops, and still writes nothing.
			if *once || *first || a.termFired() {
				if a.termFired() {
					log.Log(logs.RunStopped)
				}
				return nil
			}
			if a.waitPoll(interrupt, e.cfg.PollInterval(), log) {
				return nil
			}
		}
	}

	ob, err := outbox.Open(e.cfg.StatePath(), a.now)
	if err != nil {
		return fail(runner.Classify(err), failText(runner.Classify(err), e))
	}
	defer ob.Close()
	q, err := queue.Open(e.cfg.StatePath(), ob)
	if err != nil {
		return fail(runner.Classify(err), failText(runner.Classify(err), e))
	}
	store, err := a.keychain()
	if err != nil {
		return fail(logs.ReasonKeychain, "there is no system keychain on this platform")
	}
	secret, err := store.Get(e.cfg.KeychainAccount())
	if err != nil {
		return fail(logs.ReasonKeychain, failText(logs.ReasonKeychain, e))
	}
	instance, err := lease.Instance(e.cfg.StatePath())
	if err != nil {
		return fail(logs.ReasonState, failText(logs.ReasonState, e))
	}
	u, _ := config.SignalURL(e.cfg.Signal.URL)
	client := signal.New(u, a.transport(egress.Addresses(e.egress())), secret)
	if a.configureClient != nil {
		a.configureClient(client)
	}
	r.Outbox, r.Queue, r.Writer = ob, q, client
	r.Personal, r.Restrictions = personal(e.sources), client
	retitled, err := titled.Read(e.cfg.StatePath())
	if err != nil {
		return fail(logs.ReasonState, failText(logs.ReasonState, e))
	}
	r.Threads, r.Retitled = client, retitled
	r.Lease = &lease.Lease{Outbox: ob, Writer: client, Person: e.cfg.Person.Handle, Run: instance + ":lease",
		Period: e.cfg.LeasePeriod(), Now: a.now}

	for pass := 0; ; pass++ {
		if a.termFired() {
			log.Log(logs.RunStopped)
			return nil
		}
		a.rotateServiceLogs(stateDir)
		// Read the cursors and the first-run acceptance again before every pass, so beacon accept-first and beacon
		// cursor reset take effect on the next pass of a running agent, not only after a restart. A source the agent
		// still thinks is not accepted stores no cursor and no resume, and reads its whole open-state window again
		// every pass.
		if pass > 0 {
			if err := a.prepare(e, r, send); err != nil {
				reason := runner.Classify(err)
				return a.failLoop(*once, stateDir, poll, interrupt, log, reason, failText(reason, e), false)
			}
		}
		rep, err := r.Run(work)
		if rep != nil {
			a.printReport(rep, service.ReportBodies(*bodies, a.serviceStdout(stateDir), false))
		}
		outcome := passOutcome(err, a.termFired())
		a.recordPass(stateDir, instance, rep, outcome)
		if err != nil {
			reason := runner.Classify(err)
			var refused *lease.RefusedError
			text := failText(reason, e)
			if errors.As(err, &refused) {
				text = refused.Error() + "; check that Beacon's key has write scope and the lease subject is allowed"
			}
			return a.failLoop(*once, stateDir, poll, interrupt, log, reason, text, true)
		}
		if *once || a.termFired() {
			if a.termFired() {
				log.Log(logs.RunStopped)
			}
			return nil
		}
		if a.waitPoll(interrupt, e.cfg.PollInterval(), log) {
			return nil
		}
	}
}

// waitPoll waits for the next pass. It reports whether the loop should stop. SIGTERM is logged as a clean stop.
func (a *app) waitPoll(interrupt context.Context, d time.Duration, log *logs.Logger) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-interrupt.Done():
		return true
	case <-a.termDone():
		log.Log(logs.RunStopped)
		return true
	case <-timer.C:
		return false
	}
}

func (a *app) termFired() bool { return a.term != nil && a.term.Err() != nil }

func (a *app) termDone() <-chan struct{} {
	if a.term == nil {
		return nil
	}
	return a.term.Done()
}

func passOutcome(err error, stopped bool) string {
	if err != nil {
		reason := runner.Classify(err)
		if reason == "" {
			reason = logs.ReasonInternal
		}
		return string(reason)
	}
	if stopped {
		return "stopped"
	}
	return "ok"
}

// runFailure is an error whose text is safe to print during `beacon run`.
type runFailure struct{ text string }

func (f runFailure) Error() string { return f.text }

// failText is the message `beacon run` prints for a reason. It names no key, path or pattern.
func failText(reason logs.Reason, e *env) string {
	switch reason {
	case logs.ReasonUnauthorized:
		return "Signal refused the key (401); store a valid key with beacon key set"
	case logs.ReasonUnavailable:
		return "Signal did not answer; pending bodies are kept and sent by the next pass"
	case logs.ReasonLeaseRefused:
		return "Signal refused the lease claim"
	case logs.ReasonEgress:
		return "a request was refused because its host is not on the egress list; beacon egress lists the hosts"
	case logs.ReasonLocked:
		return "another beacon process holds the outbox in this state directory"
	case logs.ReasonKeychain:
		return fmt.Sprintf("the Signal key is not in the keychain as %s/%s; store it with beacon init", keychain.Service, e.cfg.KeychainAccount())
	case logs.ReasonState:
		return "the state directory could not be read or written; beacon outbox shows the outbox"
	case logs.ReasonCanceled:
		return "interrupted"
	case logs.ReasonSigning:
		return "refusing to run a binary whose signing identity does not match the one recorded by beacon service install"
	}
	return "the pass failed (" + string(reason) + "); beacon outbox shows what was recorded"
}

func (a *app) printReport(rep *runner.Report, bodies bool) {
	w := a.stdout
	if !rep.Send {
		fmt.Fprintln(w, "Dry run: nothing is sent.")
	}
	for _, f := range rep.Failed {
		fmt.Fprintf(w, "source %s: not collected (%s)\n", f.Name, f.Reason)
	}
	for _, name := range rep.Partial {
		if rep.Send {
			fmt.Fprintf(w, "source %s: the read stopped at a rate limit; the next send pass continues from where it stopped.\n", name)
		} else {
			fmt.Fprintf(w, "source %s: the read stopped at a rate limit. A dry run does not keep this place; the next pass reads the window from the start.\n", name)
		}
	}
	for _, name := range rep.Continued {
		if rep.Send {
			fmt.Fprintf(w, "source %s: this pass finished a scan that stopped earlier, so it lists only the remainder. The next pass reads history from here, not the window again.\n", name)
		} else {
			fmt.Fprintf(w, "source %s: this pass finished a scan that stopped earlier, so it lists only the remainder and is not a completed first run.\n", name)
		}
	}
	for _, g := range rep.Gaps {
		if g.Expired {
			fmt.Fprintf(w, "source %s: its change history expired before this pass. Beacon re-read inbox and sent mail since the last pass and starred mail from the last 14 days; a change in the gap to other mail (archived, deleted, or starred long ago) is not recovered.\n", g.Source)
		}
		if g.Cut {
			fmt.Fprintf(w, "source %s: a read stopped at its limit (Gmail: 5000 per list; Slack: 2000 per search); older items in its window were not read and will not be, unless they change.\n", g.Source)
		}
	}
	for _, wr := range rep.Warnings {
		switch wr.Code {
		case source.WarnNamesScope:
			fmt.Fprintf(w, "source %s: thread titles stay generic and items carry no names: the token lacks %s. Add them as User Token Scopes and reinstall the Slack app; the stored token gains them.\n", wr.Source, strings.Join(slack.NameScopes, ", "))
		default:
			fmt.Fprintf(w, "source %s: some display names could not be read this pass; those items carry no names or titles.\n", wr.Source)
		}
	}
	for _, p := range rep.Items {
		rule := p.Rule
		if rule == "" {
			rule = "(no rule matched: default never)"
		} else {
			rule = "rule " + rule
		}
		if p.Skipped != "" {
			// A send pass repeats every earlier decision; only a dry run lists them.
			if !rep.Send {
				fmt.Fprintf(w, "skip   %s  %s\n", p.Item.Key, p.Skipped)
			}
			continue
		}
		fmt.Fprintf(w, "%-6s %s  %s\n", p.Tier, p.Item.Key, rule)
		if p.Level != "" {
			raised := ""
			if p.Raised {
				raised = " (raised; beacon explain shows why)"
			}
			fmt.Fprintf(w, "       sealed at %s%s\n", p.Level, raised)
		}
		switch p.SealHeld {
		case logs.ReasonBodyNeedsSeal:
			fmt.Fprintln(w, "       held, not sent: its rule would send a personal body plain; a personal body leaves only sealed (body_needs_seal)")
		case logs.ReasonSensitiveShared:
			fmt.Fprintln(w, "       held, not sent: it would be sealed, and Signal seals only your own subjects (sensitive_shared)")
		}
		if p.Unaccepted {
			fmt.Fprintln(w, "       first run not accepted; not recorded. Review, then beacon accept-first")
		}
		switch p.Withheld {
		case logs.ReasonUnrestricted:
			fmt.Fprintln(w, "       held, not sent: its subject prefix is not restricted in Signal")
		case logs.ReasonRestrictionsUnknown:
			fmt.Fprintln(w, "       not sent and not recorded: Signal's restricted prefixes could not be read")
		}
		switch {
		case p.Err != nil:
			fmt.Fprintf(w, "       error: %v\n", p.Err)
		case !bodies || p.Body == nil:
		case p.Tier == rules.Send:
			fmt.Fprintf(w, "       %s\n", p.Body)
		case p.Tier == rules.Hold:
			fmt.Fprintf(w, "       held; on approval: %s\n", p.Body)
		}
	}
	s, h, n, k := rep.Counts()
	fmt.Fprintf(w, "Items: %d send, %d hold, %d never, %d already handled.\n", s, h, n, k)
	for _, wh := range rep.Withheld {
		switch wh.Reason {
		case logs.ReasonUnrestricted:
			fmt.Fprintf(w, "source %s: %d items held, not sent: %s is not restricted in Signal, so every reader could read them. Ask Signal's admin to restrict %s.\n", wh.Source, wh.Items, wh.Prefix, wh.Prefix)
		default:
			fmt.Fprintf(w, "source %s: %d items not sent and not recorded, and its cursor stays: Signal's restricted prefixes could not be read (%s). Beacon's key needs the read scope.\n", wh.Source, wh.Items, rep.RestrictionsReason)
		}
	}
	if rep.WithheldPending > 0 {
		fmt.Fprintf(w, "Pending bodies on personal subjects not sent: %d. Their prefix is not restricted in Signal, or the restrictions could not be read.\n", rep.WithheldPending)
	}
	if !rep.Send {
		return
	}
	if rep.Lease != nil {
		switch {
		case rep.Lease.Held:
			fmt.Fprintf(w, "Lease: held until %s.\n", rep.Lease.Until.UTC().Format(time.RFC3339))
		case rep.LeaseReason == "":
			fmt.Fprintln(w, "Lease: held by another instance; nothing was sent.")
		default:
			fmt.Fprintf(w, "Lease: not taken (%s); nothing was sent.\n", rep.LeaseReason)
		}
	}
	for _, res := range rep.Results {
		fmt.Fprintf(w, "%-9s %s  %d %s %s\n", res.Outcome, res.Key, res.Status, res.EventID, res.Code)
	}
	fmt.Fprintf(w, "Sent: %d. Pending: %d.\n", len(rep.Results), rep.Pending)
	if rep.Retitled > 0 {
		fmt.Fprintf(w, "Retitled: %d threads Beacon opened.\n", rep.Retitled)
	}
}

func (a *app) printFirst(rep *runner.Report) {
	by := map[string]map[string]int{}
	var names []string
	for _, p := range rep.Items {
		if by[p.Item.Source] == nil {
			by[p.Item.Source] = map[string]int{}
			names = append(names, p.Item.Source)
		}
		by[p.Item.Source][p.Item.Type]++
	}
	sort.Strings(names)
	fmt.Fprintln(a.stdout, "First run summary:")
	for _, n := range names {
		types := make([]string, 0, len(by[n]))
		for t := range by[n] {
			types = append(types, t)
		}
		sort.Strings(types)
		fmt.Fprintf(a.stdout, "  %s:", n)
		for _, t := range types {
			fmt.Fprintf(a.stdout, " %s %d", t, by[n][t])
		}
		fmt.Fprintln(a.stdout)
	}
	fmt.Fprintln(a.stdout, "First run is a dry run. Nothing is sent. Review the items, then beacon accept-first allows sending.")
}

// --- explain

func (a *app) cmdExplain(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("explain")
	bodies := fs.Bool("bodies", false, "show a sealed event's sealed parts in plaintext")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: beacon explain [--config PATH] [--bodies] KEY")
	}
	key := fs.Arg(0)
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	ob, err := outbox.Read(e.cfg.StatePath())
	if err != nil {
		return err
	}
	q, err := queue.Read(e.cfg.StatePath())
	if err != nil {
		return err
	}
	r := &runner.Runner{Sources: e.sources, Rules: e.rules, Person: e.person, Outbox: ob, Queue: q, Log: logs.Discard(), Now: a.now,
		SealOwner: &e.cfg.Seal.Owner}
	if err := a.prepare(e, r, false); err != nil {
		return err
	}
	ex, err := r.Explain(ctx, key)
	if err != nil {
		return err
	}
	w := a.stdout
	fmt.Fprintf(w, "Key:        %s\n", ex.Key)
	if ex.Item != nil {
		fmt.Fprintf(w, "Source:     %s\nSubject:    %s\nType:       %s\n", ex.Item.Source, ex.Item.Subject, ex.Item.Type)
		switch {
		case ex.Seed:
			fmt.Fprintln(w, "Rule:       seed (built-in, before the rules file)")
		case ex.Rule != nil:
			fmt.Fprintf(w, "Rule:       %d %q (line %d)\n", ex.Rule.Index, ex.Rule.Name, ex.Rule.Line)
		default:
			fmt.Fprintln(w, "Rule:       none matched; default never")
		}
		fmt.Fprintf(w, "Tier:       %s\n", ex.Tier)
		if ex.Tier != rules.Never {
			level := "plain"
			if ex.Level != "" {
				level = string(ex.Level)
			}
			fmt.Fprintf(w, "Level:      %s\n", level)
			if ex.Level != ex.RuleLevel || len(ex.Raised) > 0 {
				rl := "plain"
				if ex.RuleLevel != "" {
					rl = string(ex.RuleLevel)
				}
				fmt.Fprintf(w, "            the rule says %s; raised on this machine by:\n", rl)
				for _, why := range ex.Raised {
					fmt.Fprintf(w, "            - %s\n", why)
				}
			}
			switch ex.SealHeld {
			case logs.ReasonBodyNeedsSeal:
				fmt.Fprintln(w, "Held:       body_needs_seal: the rule would send a personal body plain; add seal to the rule")
			case logs.ReasonSensitiveShared:
				fmt.Fprintln(w, "Held:       sensitive_shared: it would be sealed on a shared subject, which Signal cannot seal")
			}
		}
	} else {
		fmt.Fprintln(w, "Item:       no source yields this key now")
	}
	fmt.Fprintln(w, "Classifier: not configured")
	if ex.Held {
		fmt.Fprintln(w, "Hold queue: held, waiting for review")
	}
	if ex.Entry != nil {
		fmt.Fprintf(w, "Outbox:     %s, appended %s", ex.Entry.State, ex.Entry.AppendedAt.Format(time.RFC3339))
		if ex.Entry.EventID != "" {
			fmt.Fprintf(w, ", event %s", ex.Entry.EventID)
		}
		if ex.Entry.Code != "" {
			fmt.Fprintf(w, ", %d %s: %s", ex.Entry.Status, ex.Entry.Code, ex.Entry.Message)
		}
		fmt.Fprintln(w)
		if !*bodies && sealedBody(ex.Body) {
			fmt.Fprintf(w, "Body (in the outbox; sealed parts hidden, --bodies shows them):\n%s\n", hideSealed(ex.Body))
			return nil
		}
		fmt.Fprintf(w, "Body (exact bytes in the outbox):\n%s\n", ex.Body)
		return nil
	}
	switch {
	case ex.Err != nil:
		fmt.Fprintf(w, "Body:       cannot be built: %v\n", ex.Err)
	case ex.Body != nil && !*bodies && sealedBody(ex.Body):
		fmt.Fprintf(w, "Body (would be sent as; sealed parts hidden, --bodies shows them):\n%s\n", hideSealed(ex.Body))
	case ex.Body != nil:
		fmt.Fprintf(w, "Body (would be sent as):\n%s\n", ex.Body)
	default:
		fmt.Fprintln(w, "Body:       none; nothing about this item leaves the machine")
	}
	return nil
}

// sealedBody reports whether a request body has a sealed part.
func sealedBody(body []byte) bool {
	var b struct {
		Sealed json.RawMessage `json:"sealed"`
	}
	return json.Unmarshal(body, &b) == nil && len(b.Sealed) > 0 && string(b.Sealed) != "null"
}

// hideSealed is a request body with each sealed part replaced by its size, so explain shows the envelope and the level
// without the plaintext.
func hideSealed(body []byte) []byte {
	var b map[string]json.RawMessage
	if json.Unmarshal(body, &b) != nil {
		return nil
	}
	var parts map[string]json.RawMessage
	if json.Unmarshal(b["sealed"], &parts) != nil {
		return nil
	}
	for k, v := range parts {
		if k != "level" {
			parts[k], _ = json.Marshal(fmt.Sprintf("[sealed: %d bytes]", len(v)))
		}
	}
	b["sealed"], _ = json.Marshal(parts)
	out, _ := json.Marshal(b)
	return out
}

// --- outbox

func (a *app) cmdOutbox(args []string) error {
	fs, cfgPath := a.flags("outbox")
	bodies := fs.Bool("bodies", false, "print each body")
	asJSON := fs.Bool("json", false, "print JSON lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ob, err := outbox.Read(cfg.StatePath())
	if err != nil {
		return err
	}
	entries := ob.Entries()
	if *asJSON {
		enc := json.NewEncoder(a.stdout)
		for _, e := range entries {
			if err := enc.Encode(map[string]any{
				"key": e.Key, "kind": e.Kind, "state": e.State, "rule": e.Rule, "event_id": e.EventID,
				"status": e.Status, "code": e.Code, "appended_at": e.AppendedAt, "updated_at": e.UpdatedAt,
				"body": json.RawMessage(e.Body),
			}); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APPENDED\tUPDATED\tSTATE\tKIND\tKEY\tEVENT\tRULE")
	for _, e := range entries {
		ev := e.EventID
		if e.State == outbox.Rejected {
			ev = fmt.Sprintf("%d %s", e.Status, e.Code)
		}
		if e.State == outbox.Superseded {
			ev = e.Code
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.AppendedAt.Format(time.RFC3339), e.UpdatedAt.Format(time.RFC3339),
			e.State, e.Kind, e.Key, dash(ev), dash(e.Rule))
		if *bodies {
			fmt.Fprintf(tw, "\t%s\n", e.Body)
		}
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- outbox retry

func (a *app) cmdOutboxRetry(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("outbox retry")
	rejected := fs.Bool("rejected", false, "re-project rejected events Signal did not store, and send them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*rejected {
		return fmt.Errorf("usage: beacon outbox retry --rejected [--config PATH]")
	}
	log := logs.New(a.stderr)
	fail := func(reason logs.Reason, text string) error {
		log.Log(logs.RunFailed, logs.Why(reason))
		return runFailure{text: text}
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return fail(logs.ReasonConfig, "the config, the rules file or a source could not be loaded; beacon rules check shows why")
	}
	if e.cfg.Mode != config.Send {
		return fail(logs.ReasonConfig, "mode is dry-run; set mode: send before retrying rejected events")
	}
	ob, err := outbox.Open(e.cfg.StatePath(), a.now)
	if err != nil {
		return fail(runner.Classify(err), failText(runner.Classify(err), e))
	}
	defer ob.Close()
	store, err := a.keychain()
	if err != nil {
		return fail(logs.ReasonKeychain, "there is no system keychain on this platform")
	}
	secret, err := store.Get(e.cfg.KeychainAccount())
	if err != nil {
		return fail(logs.ReasonKeychain, failText(logs.ReasonKeychain, e))
	}
	instance, err := lease.Instance(e.cfg.StatePath())
	if err != nil {
		return fail(logs.ReasonState, failText(logs.ReasonState, e))
	}
	u, _ := config.SignalURL(e.cfg.Signal.URL)
	client := signal.New(u, a.transport(egress.Addresses(e.egress())), secret)
	if a.configureClient != nil {
		a.configureClient(client)
	}
	sf, err := seed.ReadDir(e.cfg.StatePath())
	if err != nil {
		return fail(logs.ReasonState, failText(logs.ReasonState, e))
	}
	floor, err := seal.OpenFloor(e.cfg.StatePath())
	if err != nil {
		return fail(logs.ReasonState, failText(logs.ReasonState, e))
	}
	r := &runner.Runner{
		Rules: e.rules, Person: e.person, Outbox: ob, Writer: client, Log: log, Now: a.now, Seed: sf, Floor: floor,
		SealOwner: &e.cfg.Seal.Owner,
		Personal: personal(e.sources), Restrictions: client,
		Lease: &lease.Lease{Outbox: ob, Writer: client, Person: e.cfg.Person.Handle, Run: instance + ":lease",
			Period: e.cfg.LeasePeriod(), Now: a.now},
	}
	rep, err := r.RetryRejected(ctx)
	a.printRetry(rep)
	if err != nil {
		reason := runner.Classify(err)
		var refused *lease.RefusedError
		if errors.As(err, &refused) {
			return fail(reason, refused.Error()+"; check that Beacon's key has write scope and the lease subject is allowed")
		}
		return fail(reason, failText(reason, e))
	}
	return nil
}

// printRetry writes counts only. It does not print keys, bodies or error text.
func (a *app) printRetry(rep *runner.RetryReport) {
	if rep == nil {
		return
	}
	w := a.stdout
	if rep.Lease != nil {
		switch {
		case rep.Lease.Held:
			fmt.Fprintf(w, "Lease: held until %s.\n", rep.Lease.Until.UTC().Format(time.RFC3339))
		case rep.LeaseReason == "":
			fmt.Fprintln(w, "Lease: held by another instance; nothing was sent.")
		case rep.LeaseReason != logs.ReasonLeaseExpired:
			fmt.Fprintf(w, "Lease: not taken (%s); nothing was sent.\n", rep.LeaseReason)
		}
	}
	fmt.Fprintf(w, "Retry: sent %d, replayed %d, refused %d, conflict %d, missing %d, failed %d, hold %d, never %d, withheld %d.\n",
		rep.Sent, rep.Replayed, rep.Refused, rep.Conflict, rep.Missing, rep.Failed, rep.Held, rep.Never, rep.Withheld)
	if rep.Withheld > 0 {
		fmt.Fprintf(w, "%d rejected events on personal subjects were not sent: their prefix is not restricted in Signal, or the restrictions could not be read.\n", rep.Withheld)
	}
	fmt.Fprintf(w, "Pending: %d.\n", rep.Pending)
	if rep.Missing > 0 {
		fmt.Fprintf(w, "%d rejected events have no stored item and were not sent by this command. beacon cursor reset SOURCE, then beacon run --once, re-collects open state and sends a 400 if that collect yields the key. It does not re-read history. A 409 is not sent.\n", rep.Missing)
	}
}

// --- cursor reset

func (a *app) cmdCursor(args []string) error {
	if len(args) == 0 || args[0] != "reset" {
		fmt.Fprint(a.stderr, "usage: beacon cursor reset [--config PATH] SOURCE...\n")
		return exitError{code: 2}
	}
	fs, cfgPath := a.flags("cursor reset")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fmt.Fprint(a.stderr, "usage: beacon cursor reset [--config PATH] SOURCE...\n")
		return exitError{code: 2}
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, s := range e.sources {
		known[s.Name()] = true
	}
	for _, n := range fs.Args() {
		if !known[n] {
			return fmt.Errorf("no source is named %q", n)
		}
	}
	ob, err := outbox.Open(e.cfg.StatePath(), a.now)
	if err != nil {
		return err
	}
	defer ob.Close()
	st, err := cursor.Open(e.cfg.StatePath())
	if err != nil {
		return err
	}
	for _, n := range fs.Args() {
		cleared, err := st.Reset(n)
		if err != nil {
			return err
		}
		if cleared {
			fmt.Fprintf(a.stdout, "Cleared the cursor for %s. The next collect reads open state, not history. beacon run --once sends a 400 with no stored item if that collect yields the key. A 409, and anything Signal accepted, is not.\n", n)
		} else {
			fmt.Fprintf(a.stdout, "%s has no cursor. The next collect already reads open state, not history. beacon run --once sends a 400 with no stored item if that collect yields the key. A 409, and anything Signal accepted, is not.\n", n)
		}
	}
	return nil
}

// --- egress

func (a *app) cmdEgress(args []string) error {
	fs, cfgPath := a.flags("egress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	for _, h := range e.egress() {
		fmt.Fprintf(tw, "%s\t%s\n", h.Address, h.Purpose)
	}
	return tw.Flush()
}

// --- rules check

func (a *app) cmdRulesCheck(args []string) error {
	fs, cfgPath := a.flags("rules check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	problems := e.rules.Check(e.checkSources())
	failed := false
	for _, p := range problems {
		fmt.Fprintln(a.stdout, p)
		failed = failed || !p.Warning
	}
	names := make([]string, 0, len(e.sources))
	for _, s := range e.sources {
		names = append(names, s.Name())
	}
	sort.Strings(names)
	if failed {
		return exitError{code: 1}
	}
	fmt.Fprintf(a.stdout, "%s: %d rules, %d warnings, no errors (sources: %s)\n", e.cfg.RulesPath(), len(e.rules.Rules), len(problems), strings.Join(names, ", "))
	return nil
}

// --- version

func (a *app) cmdVersion() error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "beacon %s\n", version)
	c := commit
	settings := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
		if c == "" {
			c = settings["vcs.revision"]
			if settings["vcs.modified"] == "true" {
				c += "+modified"
			}
		}
	}
	fmt.Fprintf(&b, "commit:    %s\n", dash(c))
	fmt.Fprintf(&b, "go:        %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	for _, k := range []string{"-trimpath", "-ldflags", "-tags", "CGO_ENABLED", "-buildmode"} {
		if v, ok := settings[k]; ok {
			fmt.Fprintf(&b, "%-10s %s\n", k+":", v)
		}
	}
	_, err := a.stdout.Write(b.Bytes())
	return err
}
