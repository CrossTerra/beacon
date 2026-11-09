package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/github"
	"github.com/CrossTerra/beacon/internal/jira"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/seed"
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
	for _, s := range cfg.Sources {
		var src source.Source
		switch s.Kind {
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

// prepare loads the cursor, the seed and the first-run acceptance. A send pass may commit cursors; a dry run must not.
func (a *app) prepare(e *env, r *runner.Runner, send bool) error {
	var st *cursor.Store
	var err error
	if send {
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
		switch s.(type) {
		case *github.Source, *jira.Source:
			if !st.Accepted(s.Name()) {
				r.Unaccepted[s.Name()] = true
			}
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
		return fmt.Errorf("usage: beacon source add github|jira [flags]")
	}
	switch args[1] {
	case "github":
		return a.addGitHub(ctx, args[2:])
	case "jira":
		return a.addJira(ctx, args[2:])
	default:
		return fmt.Errorf("usage: beacon source add github|jira [flags]")
	}
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
	token, err := a.readGitHubToken(ctx, *fromGH)
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
	fmt.Fprint(a.stderr, "Atlassian account email: ")
	email, err := readLine(a.stdin)
	if err != nil {
		return err
	}
	fmt.Fprint(a.stderr, "API token (input hidden): ")
	token, err := keychain.ReadSecret(ctx, a.stdin)
	fmt.Fprintln(a.stderr)
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

func (a *app) readGitHubToken(ctx context.Context, fromGH bool) (keychain.Secret, error) {
	if !fromGH {
		fmt.Fprint(a.stderr, "GitHub token (input hidden): ")
		s, err := keychain.ReadSecret(ctx, a.stdin)
		fmt.Fprintln(a.stderr)
		return s, err
	}
	var raw string
	var err error
	if a.ghToken != nil {
		raw, err = a.ghToken(ctx)
	} else {
		raw, err = ghAuthToken(ctx)
	}
	if err != nil {
		return keychain.Secret{}, err
	}
	return keychain.FromString(raw)
}

func ghAuthToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
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
	line, err := bufio.NewReader(r).ReadString('\n')
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
			switch s.(type) {
			case *github.Source, *jira.Source:
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
	if err := st.Accept(names); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Accepted the first run of %s. A send pass may now send their items.\n", strings.Join(names, ", "))
	return nil
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
