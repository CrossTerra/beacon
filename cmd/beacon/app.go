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
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/source"
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
}

const usage = `usage: beacon <command> [flags]

Commands:
  init          write a config and an empty rules file, and store the Signal key in the keychain
  run           collect, apply rules and projection; print what would be sent (dry run), or send
  explain KEY   show the matching rule, the classifier's answer and the exact body for one item
  outbox        list what was sent, with keys, times and Signal event ids
  egress        print every host Beacon may contact under the current config
  rules check   validate the rules file and report rules that can never match
  version       print the version, commit and build flags

Every command but version takes --config PATH (default: $BEACON_CONFIG, else the user config directory).
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
		err = a.cmdInit(rest)
	case "run":
		err = a.cmdRun(ctx, rest)
	case "explain":
		err = a.cmdExplain(ctx, rest)
	case "outbox":
		err = a.cmdOutbox(rest)
	case "egress":
		err = a.cmdEgress(rest)
	case "rules":
		if len(rest) == 0 || rest[0] != "check" {
			fmt.Fprint(a.stderr, "usage: beacon rules check [--config PATH]\n")
			return 2
		}
		err = a.cmdRulesCheck(rest[1:])
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
	names := map[string]bool{}
	for _, s := range cfg.Sources {
		var src source.Source
		switch s.Kind {
		case "fixture":
			f, err := source.LoadFixture(cfg.Resolve(s.Path))
			if err != nil {
				return nil, err
			}
			src = f
		}
		if names[src.Name()] {
			return nil, fmt.Errorf("two sources are named %q", src.Name())
		}
		names[src.Name()] = true
		e.sources = append(e.sources, src)
	}
	e.rules, err = rules.Load(cfg.RulesPath())
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (e *env) sourceFields() map[string][]string {
	out := map[string][]string{}
	for _, s := range e.sources {
		out[s.Name()] = s.Fields()
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

func (a *app) cmdInit(args []string) error {
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
	secret, err := keychain.ReadSecret(a.stdin)
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

// --- run

func (a *app) cmdRun(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("run")
	dry := fs.Bool("dry-run", false, "print what would be sent, and send nothing (the default until the config says mode: send)")
	once := fs.Bool("once", false, "make one pass and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	if problems := errorsOnly(e.rules.Check(e.sourceFields())); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(a.stderr, p)
		}
		return fmt.Errorf("the rules file has errors; run beacon rules check")
	}
	send := e.cfg.Mode == config.Send && !*dry
	log := logs.New(a.stderr)
	r := &runner.Runner{Sources: e.sources, Rules: e.rules, Person: e.person, Send: send, Log: log, Now: a.now,
		AfterAppend: a.afterAppend}

	if !send {
		ob, err := outbox.Read(e.cfg.StatePath())
		if err != nil {
			return err
		}
		q, err := queue.Open(e.cfg.StatePath())
		if err != nil {
			return err
		}
		r.Outbox, r.Queue = ob, q
		r.Send = false
		rep, err := r.Run(ctx)
		if err != nil {
			return err
		}
		a.printReport(rep)
		return nil
	}

	ob, err := outbox.Open(e.cfg.StatePath(), a.now)
	if err != nil {
		return err
	}
	defer ob.Close()
	q, err := queue.Open(e.cfg.StatePath())
	if err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	secret, err := store.Get(e.cfg.KeychainAccount())
	if err != nil {
		return fmt.Errorf("reading the Signal key (%s/%s): %w", keychain.Service, e.cfg.KeychainAccount(), err)
	}
	instance, err := lease.Instance(e.cfg.StatePath())
	if err != nil {
		return err
	}
	u, _ := config.SignalURL(e.cfg.Signal.URL)
	client := signal.New(u, a.transport(egress.Addresses(e.egress())), secret)
	if a.configureClient != nil {
		a.configureClient(client)
	}
	r.Outbox, r.Queue, r.Writer = ob, q, client
	r.Lease = &lease.Lease{Outbox: ob, Writer: client, Person: e.cfg.Person.Handle, Run: instance + ":lease",
		Period: e.cfg.LeasePeriod(), Now: a.now}

	for {
		rep, err := r.Run(ctx)
		if rep != nil {
			a.printReport(rep)
		}
		if err != nil {
			return err
		}
		if *once {
			return nil
		}
		t := time.NewTimer(e.cfg.PollInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func errorsOnly(ps []rules.Problem) []rules.Problem {
	var out []rules.Problem
	for _, p := range ps {
		if !p.Unreachable {
			out = append(out, p)
		}
	}
	return out
}

func (a *app) printReport(rep *runner.Report) {
	w := a.stdout
	if !rep.Send {
		fmt.Fprintln(w, "Dry run: nothing is sent.")
	}
	for _, p := range rep.Items {
		rule := p.Rule
		if rule == "" {
			rule = "(no rule matched: default never)"
		} else {
			rule = "rule " + rule
		}
		switch {
		case p.Skipped != "":
			fmt.Fprintf(w, "skip   %s  %s\n", p.Item.Key, p.Skipped)
			continue
		default:
			fmt.Fprintf(w, "%-6s %s  %s\n", p.Tier, p.Item.Key, rule)
		}
		if p.Err != nil {
			fmt.Fprintf(w, "       error: %v\n", p.Err)
		} else if p.Tier == rules.Send && p.Body != nil {
			fmt.Fprintf(w, "       %s\n", p.Body)
		} else if p.Tier == rules.Hold && p.Body != nil {
			fmt.Fprintf(w, "       held; on approval: %s\n", p.Body)
		}
	}
	s, h, n, k := rep.Counts()
	fmt.Fprintf(w, "Items: %d send, %d hold, %d never, %d already handled.\n", s, h, n, k)
	if !rep.Send {
		return
	}
	if rep.Lease != nil {
		if rep.Lease.Held {
			fmt.Fprintf(w, "Lease: held until %s.\n", rep.Lease.Until.UTC().Format(time.RFC3339))
		} else {
			fmt.Fprintln(w, "Lease: held by another instance; nothing was sent.")
		}
	}
	for _, res := range rep.Results {
		fmt.Fprintf(w, "%-9s %s  %d %s %s\n", res.Outcome, res.Key, res.Status, res.EventID, res.Code)
	}
	fmt.Fprintf(w, "Sent: %d. Pending: %d.\n", len(rep.Results), rep.Pending)
}

// --- explain

func (a *app) cmdExplain(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("explain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: beacon explain [--config PATH] KEY")
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
	q, err := queue.Open(e.cfg.StatePath())
	if err != nil {
		return err
	}
	r := &runner.Runner{Sources: e.sources, Rules: e.rules, Person: e.person, Outbox: ob, Queue: q, Log: logs.Discard(), Now: a.now}
	ex, err := r.Explain(ctx, key)
	if err != nil {
		return err
	}
	w := a.stdout
	fmt.Fprintf(w, "Key:        %s\n", ex.Key)
	if ex.Item != nil {
		fmt.Fprintf(w, "Source:     %s\nSubject:    %s\nType:       %s\n", ex.Item.Source, ex.Item.Subject, ex.Item.Type)
		if ex.Rule != nil {
			fmt.Fprintf(w, "Rule:       %d %q (line %d)\n", ex.Rule.Index, ex.Rule.Name, ex.Rule.Line)
		} else {
			fmt.Fprintln(w, "Rule:       none matched; default never")
		}
		fmt.Fprintf(w, "Tier:       %s\n", ex.Tier)
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
		fmt.Fprintf(w, "Body (exact bytes in the outbox):\n%s\n", ex.Body)
		return nil
	}
	switch {
	case ex.Err != nil:
		fmt.Fprintf(w, "Body:       cannot be built: %v\n", ex.Err)
	case ex.Body != nil:
		fmt.Fprintf(w, "Body (would be sent as):\n%s\n", ex.Body)
	default:
		fmt.Fprintln(w, "Body:       none; nothing about this item leaves the machine")
	}
	return nil
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
	problems := e.rules.Check(e.sourceFields())
	for _, p := range problems {
		fmt.Fprintln(a.stdout, p)
	}
	names := make([]string, 0, len(e.sources))
	for _, s := range e.sources {
		names = append(names, s.Name())
	}
	sort.Strings(names)
	if len(problems) > 0 {
		return exitError{code: 1}
	}
	fmt.Fprintf(a.stdout, "%s: %d rules, no problems (sources: %s)\n", e.cfg.RulesPath(), len(e.rules.Rules), strings.Join(names, ", "))
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
