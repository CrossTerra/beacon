package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/github"
	"github.com/CrossTerra/beacon/internal/google"
	"github.com/CrossTerra/beacon/internal/jira"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/seed"
	"github.com/CrossTerra/beacon/internal/slack"
	"github.com/CrossTerra/beacon/internal/source"
)

func (a *app) buildSources(cfg *config.Config) ([]source.Source, error) {
	var jiraProjects []string
	for _, s := range cfg.Sources {
		if s.Kind == "jira" {
			jiraProjects = append(jiraProjects, s.Projects...)
		}
	}
	var out []source.Source
	names := map[string]bool{}
	// One grant per Google account, shared by that account's sources.
	auths := map[string]*google.Auth{}
	for _, s := range cfg.Sources {
		var src source.Source
		if s.IsGoogle() && auths[s.Account] == nil {
			auth, err := google.NewAuth(google.AuthOptions{
				TokenURL: a.google.tokenURL,
				Client:   a.secretFunc(keychain.GoogleClientAccount(s.Account)),
				Refresh:  a.secretFunc(keychain.GoogleRefreshAccount(s.Account)),
				Now:      a.now,
			})
			if err != nil {
				return nil, err
			}
			auths[s.Account] = auth
		}
		gopts := google.Options{
			Name: s.SourceName(), Person: strings.TrimPrefix(cfg.Person.Handle, "@"), Account: s.Account,
			Identity: cfg.Person.Identities[s.SourceName()], Auth: auths[s.Account], Now: a.now,
		}
		switch s.Kind {
		case "gmail":
			gopts.Base = a.google.gmailBase
			g, err := google.NewGmail(gopts)
			if err != nil {
				return nil, err
			}
			src = g
		case "calendar":
			gopts.Base = a.google.apiBase
			c, err := google.NewCalendar(gopts)
			if err != nil {
				return nil, err
			}
			src = c
		case "drive":
			gopts.Base = a.google.apiBase
			d, err := google.NewDrive(gopts)
			if err != nil {
				return nil, err
			}
			src = d
		case "slack":
			sl, err := slack.New(slack.Options{
				Name: s.SourceName(), Person: strings.TrimPrefix(cfg.Person.Handle, "@"),
				Workspace: s.Workspace, User: s.User, Channels: s.Channels,
				Identity: cfg.Person.Identities[s.SourceName()],
				Token:    a.secretFunc(keychain.SlackAccount(s.Workspace, s.User)),
				Cache:    filepath.Join(cfg.StatePath(), slack.CacheFile(s.SourceName())),
				Base:     a.slackBase, Now: a.now, Sleep: a.sleep,
			})
			if err != nil {
				return nil, err
			}
			src = sl
		case "fixture":
			f, err := source.LoadFixture(cfg.Resolve(s.Path))
			if err != nil {
				return nil, err
			}
			src = f
		case "github":
			projects := append([]string{}, s.Projects...)
			seen := map[string]bool{}
			for _, p := range projects {
				seen[p] = true
			}
			for _, p := range jiraProjects {
				if !seen[p] {
					projects = append(projects, p)
					seen[p] = true
				}
			}
			g, err := github.New(github.Options{
				Name: s.SourceName(), Host: s.Host, Repos: s.Repos, Projects: projects,
				Identity: cfg.Person.Identities[s.SourceName()],
				Token:    a.secretFunc(keychain.SourceAccount(s.SourceName())),
				Now:      a.now,
			})
			if err != nil {
				return nil, err
			}
			src = g
		case "jira":
			j, err := jira.New(jira.Options{
				Name: s.SourceName(), Site: s.Site, Projects: s.Projects,
				Identity: cfg.Person.Identities[s.SourceName()],
				Creds:    a.secretFunc(keychain.SourceAccount(s.SourceName())),
				Now:      a.now,
			})
			if err != nil {
				return nil, err
			}
			src = j
		default:
			return nil, fmt.Errorf("unknown source kind %q", s.Kind)
		}
		if names[src.Name()] {
			return nil, fmt.Errorf("two sources are named %q", src.Name())
		}
		names[src.Name()] = true
		out = append(out, src)
	}
	return out, nil
}

// live reports whether a source reads a real service. Its first run reads open state and is a dry run until
// beacon accept-first (DEC-23). Only the fixture is not.
func live(s source.Source) bool {
	_, fixture := s.(*source.Fixture)
	return !fixture
}

// personal maps each personal source to its subject stem. A send waits until Signal restricts that stem.
func personal(sources []source.Source) map[string]string {
	out := map[string]string{}
	for _, s := range sources {
		if p, ok := s.(interface{ PersonalStem() string }); ok {
			out[s.Name()] = p.PersonalStem()
		}
	}
	return out
}

func (a *app) secretFunc(account string) func() (keychain.Secret, error) {
	return func() (keychain.Secret, error) {
		if a.keychain == nil {
			return keychain.Secret{}, keychain.ErrNotFound
		}
		store, err := a.keychain()
		if err != nil {
			return keychain.Secret{}, err
		}
		return store.Get(account)
	}
}

// prepare loads the cursor, the seed and the first-run acceptance. A send pass may commit a cursor. A dry run
// and explain open the store read-only: they write no cursor, so no lock is taken.
func (a *app) prepare(e *env, r *runner.Runner, send bool) error {
	return a.prepareStore(e, r, send)
}

func (a *app) prepareStore(e *env, r *runner.Runner, writable bool) error {
	var st *cursor.Store
	var err error
	if writable {
		st, err = cursor.Open(e.cfg.StatePath())
	} else {
		st, err = cursor.Read(e.cfg.StatePath())
	}
	if err != nil {
		return err
	}
	r.Cursors = st
	r.Unaccepted = map[string]bool{}
	for _, s := range e.sources {
		if live(s) && !st.Accepted(s.Name()) {
			r.Unaccepted[s.Name()] = true
		}
	}
	sf, err := seed.ReadDir(e.cfg.StatePath())
	if err != nil {
		return err
	}
	r.Seed = sf
	return nil
}

func (a *app) cmdSource(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "add" {
		return fmt.Errorf("usage: beacon source add github|jira|google|slack [flags]")
	}
	switch args[1] {
	case "slack":
		return a.addSlack(ctx, args[2:])
	case "github":
		return a.addGitHub(ctx, args[2:])
	case "jira":
		return a.addJira(ctx, args[2:])
	case "google":
		return a.addGoogle(ctx, args[2:])
	default:
		return fmt.Errorf("usage: beacon source add github|jira|google|slack [flags]")
	}
}

// addSlack reads the person's user token, checks it with auth.test, stores it under the workspace and user, and
// enables the source. Beacon calls only read methods with it.
func (a *app) addSlack(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("source add slack")
	var channels repeatFlag
	fs.Var(&channels, "channel", "channel id (C…) whose top-level messages are read for review requests (repeatable)")
	name := fs.String("name", "slack", "source name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon source add slack [--channel ID]... [--name NAME]")
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(cfg.Person.Handle, "@") {
		return errors.New("a slack source needs person.handle as @handle: direct-message subjects carry it (DEC-10)")
	}
	fmt.Fprint(a.stderr, "Slack user token, xoxp-… (input hidden): ")
	token, err := keychain.ReadSecret(ctx, a.stdin)
	fmt.Fprintln(a.stderr)
	if err != nil {
		return err
	}
	if err := keychain.SlackUserToken(token); err != nil {
		return err
	}
	transport := a.transport
	if transport == nil {
		transport = egress.Transport
	}
	base := a.slackBase
	if base == "" {
		base = slack.Base
	}
	id, err := slack.Verify(ctx, limited.HTTPClient(transport(slack.Hosts(base))), base, token)
	if err != nil {
		if errors.Is(err, source.ErrUnauthorized) {
			return errors.New("slack refused the token, or it is not a user token; nothing was stored")
		}
		return err
	}
	if missing := append(append([]string(nil), id.Missing...), id.MissingNames...); len(missing) > 0 {
		return fmt.Errorf("the token lacks the scopes %s; add them as User Token Scopes, reinstall the app, and run this again. Nothing was stored", strings.Join(missing, ", "))
	}
	src := config.Source{Kind: "slack", Name: *name, Workspace: id.Workspace, User: id.User, Channels: channels}
	if src.Name == "slack" {
		src.Name = ""
	}
	cfg.Sources = upsert(cfg.Sources, src)
	if err := validateConfig(cfg); err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	account := keychain.SlackAccount(id.Workspace, id.User)
	if err := store.Set(account, token); err != nil {
		return err
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Stored the Slack token in the keychain as %s/%s\nEnabled source %s (workspace %s, user %s)\n",
		keychain.Service, account, src.SourceName(), id.Workspace, id.User)
	if len(id.Extra) > 0 {
		fmt.Fprintf(a.stdout, "The token also holds %s. Beacon never calls a method that uses them; a token with only the read scopes is safer.\n", strings.Join(id.Extra, ", "))
	}
	if cfg.Person.Identities[src.SourceName()] == "" {
		fmt.Fprintf(a.stdout, "Set person.identities.%s: slack:user:%s so your own messages carry by.\n", src.SourceName(), id.User)
	}
	return nil
}

// addGoogle stores the person's OAuth client, runs the installed-app flow, stores the refresh token, and enables
// the chosen Google sources. Every scope is read-only.
func (a *app) addGoogle(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("source add google")
	account := fs.String("account", "", "the Google account to read, its primary address (required)")
	clientFile := fs.String("client-file", "", "OAuth client JSON of a Desktop app from your Google Cloud project; stored in the keychain (default: the client already stored)")
	gmail := fs.Bool("gmail", false, "enable Gmail")
	calendar := fs.Bool("calendar", false, "enable Calendar")
	drive := fs.Bool("drive", false, "enable Drive comments and meeting notes")
	bodies := fs.Bool("bodies", false, "grant gmail.readonly instead of gmail.metadata, so a rule that lists body can send a mail body")
	open := fs.Bool("open", false, "open the consent URL in the default browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon source add google --account EMAIL [--client-file FILE] [--gmail] [--calendar] [--drive] [--bodies] [--open]")
	}
	email := strings.ToLower(strings.TrimSpace(*account))
	if email == "" {
		return errors.New("pass --account with the Google account's address")
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	chosen := map[string]bool{"gmail": *gmail, "calendar": *calendar, "drive": *drive}
	if !*gmail && !*calendar && !*drive {
		chosen = map[string]bool{"gmail": true, "calendar": true, "drive": true}
	}
	for _, k := range config.GoogleKinds {
		if chosen[k] {
			cfg.Sources = upsert(cfg.Sources, config.Source{Kind: k, Account: email})
		}
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	// One grant serves every Google source in the config that reads this account, so it asks for all their scopes.
	var kinds, names []string
	for _, s := range cfg.Sources {
		if s.IsGoogle() && s.Account == email {
			kinds = append(kinds, s.Kind)
			names = append(names, s.SourceName())
		}
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	var client keychain.Secret
	if *clientFile != "" {
		data, err := os.ReadFile(*clientFile)
		if err != nil {
			return err
		}
		client, err = keychain.OAuthClient(data)
		if err != nil {
			return err
		}
		if err := store.Set(keychain.GoogleClientAccount(email), client); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Stored the OAuth client in the keychain as %s/%s\n", keychain.Service, keychain.GoogleClientAccount(email))
	} else {
		client, err = store.Get(keychain.GoogleClientAccount(email))
		if errors.Is(err, keychain.ErrNotFound) {
			return errors.New("no Google OAuth client is stored for that account; pass --client-file with the JSON of a Desktop app client")
		}
		if err != nil {
			return err
		}
	}
	srcs, err := a.buildSources(cfg)
	if err != nil {
		return err
	}
	u, _ := config.SignalURL(cfg.Signal.URL)
	transport := a.transport
	if transport == nil {
		transport = egress.Transport
	}
	flow := google.Flow{
		AuthURL: a.google.authURL, TokenURL: a.google.tokenURL,
		HTTP:   limited.HTTPClient(transport(egress.Addresses(egress.List(u, srcs)))),
		Scopes: google.Scopes(kinds, *bodies),
		Out:    a.stderr, LoginHint: email,
	}
	if *open {
		flow.Open = a.google.open
		if flow.Open == nil {
			flow.Open = openBrowser
		}
	}
	tok, err := flow.Run(ctx, client)
	if err != nil {
		return err
	}
	if err := store.Set(keychain.GoogleRefreshAccount(email), tok.Refresh); err != nil {
		return err
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Stored the Google refresh token in the keychain as %s/%s\nGranted read-only: %s\nEnabled sources %s\n",
		keychain.Service, keychain.GoogleRefreshAccount(email), strings.Join(tok.Scopes, " "), strings.Join(names, ", "))
	return nil
}

// openBrowser opens a URL with the system's opener. The browser, not Beacon, contacts the consent page.
func openBrowser(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Start()
}

type repeatFlag []string

func (r *repeatFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func (a *app) addGitHub(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("source add github")
	var repos, projects repeatFlag
	fs.Var(&repos, "repo", "repository owner/name or owner/* (repeatable)")
	fs.Var(&projects, "project", "Jira project key to recognize in titles and branches (repeatable)")
	host := fs.String("host", "", "GitHub API host (default api.github.com; an enterprise host uses /api/v3)")
	name := fs.String("name", "github", "source name")
	fromGH := fs.Bool("from-gh", false, "import the token `gh auth token` prints")
	yes := fs.Bool("yes", false, "confirm the import; required when input is not a terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	src := config.Source{Kind: "github", Name: *name, Host: *host, Repos: repos, Projects: projects}
	if src.Name == "github" {
		src.Name = ""
	}
	cfg.Sources = upsert(cfg.Sources, src)
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if *fromGH {
		if err := a.confirm("Import the token from `gh auth token` into the keychain? [y/N] ", *yes); err != nil {
			return err
		}
	}
	token, err := a.readGitHubToken(ctx, *fromGH, *host)
	if err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	account := keychain.SourceAccount(src.SourceName())
	if err := store.Set(account, token); err != nil {
		return err
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Stored the GitHub token in the keychain as %s/%s\nEnabled source %s\n", keychain.Service, account, src.SourceName())
	return nil
}

func (a *app) addJira(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("source add jira")
	var projects repeatFlag
	fs.Var(&projects, "project", "project key (repeatable)")
	site := fs.String("site", "", "Jira Cloud site label, the <site> in <site>.atlassian.net")
	name := fs.String("name", "jira", "source name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	src := config.Source{Kind: "jira", Name: *name, Site: *site, Projects: projects}
	if src.Name == "jira" {
		src.Name = ""
	}
	cfg.Sources = upsert(cfg.Sources, src)
	if err := validateConfig(cfg); err != nil {
		return err
	}
	// One reader for a pipe: a second bufio.Reader would miss the token, which the first reader already buffered.
	in := bufio.NewReader(a.stdin)
	fmt.Fprint(a.stderr, "Atlassian account email: ")
	email, err := readLineFrom(in)
	if err != nil {
		return err
	}
	fmt.Fprint(a.stderr, "API token (input hidden): ")
	var token keychain.Secret
	if isTerminal(a.stdin) {
		token, err = keychain.ReadSecret(ctx, a.stdin)
		fmt.Fprintln(a.stderr)
	} else {
		var line string
		line, err = readLineFrom(in)
		if err == nil {
			token, err = keychain.FromString(line)
		}
	}
	if err != nil {
		return err
	}
	secret, err := keychain.PairWith(email, token)
	if err != nil {
		return err
	}
	store, err := a.keychain()
	if err != nil {
		return err
	}
	account := keychain.SourceAccount(src.SourceName())
	if err := store.Set(account, secret); err != nil {
		return err
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Stored the Jira credential in the keychain as %s/%s\nEnabled source %s (%s.atlassian.net)\n", keychain.Service, account, src.SourceName(), *site)
	return nil
}

func (a *app) readGitHubToken(ctx context.Context, fromGH bool, host string) (keychain.Secret, error) {
	if !fromGH {
		fmt.Fprint(a.stderr, "GitHub token (input hidden): ")
		s, err := keychain.ReadSecret(ctx, a.stdin)
		fmt.Fprintln(a.stderr)
		return s, err
	}
	var raw string
	var err error
	if a.ghToken != nil {
		raw, err = a.ghToken(ctx, host)
	} else {
		raw, err = ghAuthToken(ctx, host)
	}
	if err != nil {
		return keychain.Secret{}, err
	}
	return keychain.FromString(raw)
}

func splitHost(host string) (string, string, error) {
	if !strings.Contains(host, ":") {
		return host, "", nil
	}
	return net.SplitHostPort(host)
}

func ghAuthToken(ctx context.Context, host string) (string, error) {
	args := []string{"auth", "token"}
	if host != "" {
		h, _, err := splitHost(host)
		if err != nil {
			h = host
		}
		args = append(args, "--hostname", h)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("gh auth token failed; run gh auth login")
	}
	return strings.TrimSpace(string(out)), nil
}

func (a *app) confirm(prompt string, yes bool) error {
	if yes {
		return nil
	}
	if !isTerminal(a.stdin) {
		return errors.New("pass --yes to confirm")
	}
	fmt.Fprint(a.stderr, prompt)
	line, err := readLine(a.stdin)
	if err != nil {
		return err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return nil
	default:
		return errors.New("not confirmed")
	}
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func readLine(r io.Reader) (string, error) {
	return readLineFrom(bufio.NewReader(r))
}

func readLineFrom(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (a *app) cmdSeed(args []string) error {
	fs, cfgPath := a.flags("seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: beacon seed [--config PATH] FILE")
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	f, err := seed.Write(cfg.StatePath(), data)
	if err != nil {
		return err
	}
	n, s := f.Counts()
	fmt.Fprintf(a.stdout, "Imported %d never and %d send. A never line drops that subject before the rules file. A send line does not force a send.\n", n, s)
	return nil
}

func (a *app) cmdAcceptFirst(args []string) error {
	fs, cfgPath := a.flags("accept-first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	names := fs.Args()
	if len(names) == 0 {
		for _, s := range e.sources {
			if live(s) {
				names = append(names, s.Name())
			}
		}
	}
	if len(names) == 0 {
		return errors.New("no source to accept; add one with beacon source add")
	}
	known := map[string]bool{}
	for _, s := range e.sources {
		known[s.Name()] = true
	}
	for _, n := range names {
		if !known[n] {
			return fmt.Errorf("no source is named %q", n)
		}
	}
	st, err := cursor.Open(e.cfg.StatePath())
	if err != nil {
		return err
	}
	for _, n := range names {
		if !st.FirstDone(n) {
			return fmt.Errorf("%s has no completed beacon run --first; review that summary first", n)
		}
	}
	if err := st.Accept(names); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Accepted the first run of %s. A send pass may now send their items.\n", strings.Join(names, ", "))
	return nil
}

func (a *app) recordFirst(e *env, rep *runner.Report) error {
	failed := map[string]bool{}
	for _, f := range rep.Failed {
		failed[f.Name] = true
	}
	partial := map[string]bool{}
	for _, n := range append(rep.Partial, rep.Continued...) {
		partial[n] = true
	}
	var names []string
	for _, s := range e.sources {
		// A read that stopped at a rate limit, or that only finished a scan an earlier pass started, is not a
		// completed summary: the items are not the whole window.
		if live(s) && !failed[s.Name()] && !partial[s.Name()] {
			names = append(names, s.Name())
		}
	}
	if len(names) == 0 {
		return nil
	}
	st, err := cursor.Open(e.cfg.StatePath())
	if err != nil {
		return err
	}
	return st.MarkFirst(names)
}

func upsert(sources []config.Source, s config.Source) []config.Source {
	name := s.SourceName()
	for i := range sources {
		if sources[i].SourceName() == name {
			sources[i] = s
			return sources
		}
	}
	return append(sources, s)
}

func validateConfig(cfg *config.Config) error {
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	_, err = config.Parse(data)
	return err
}

func writeConfig(cfg *config.Config) error {
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfg.Path)
	tmp, err := os.CreateTemp(dir, "config.yaml.tmp")
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
	if err := os.Rename(tmpName, cfg.Path); err != nil {
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
