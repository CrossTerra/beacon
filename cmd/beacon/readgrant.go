package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/slack"
)

func (a *app) grantClient(hosts []string) *http.Client {
	transport := a.transport
	if transport == nil {
		transport = egress.Transport
	}
	return &http.Client{Transport: egress.Guard(hosts, transport(hosts)), Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (a *app) checkGitHubReadGrant(ctx context.Context, origin string, token keychain.Secret, newReadToken bool) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || origin != "https://"+u.Host || u.User != nil {
		return errors.New("invalid GitHub API origin")
	}
	if !keychain.ActionToken(token) {
		return errors.New("invalid GitHub token")
	}
	path := "/user"
	if !strings.EqualFold(u.Hostname(), "api.github.com") {
		path = "/api/v3/user"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
	if err != nil {
		return errors.New("invalid GitHub token verification endpoint")
	}
	r.Header.Set("Accept", "application/vnd.github+json")
	token.Authorize(r.Header)
	c := a.grantClient([]string{egress.Address(u)})
	defer c.CloseIdleConnections()
	res, err := c.Do(r)
	if err != nil {
		return errors.New("could not verify the GitHub token with its configured origin")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("GitHub did not verify the token; nothing was stored")
	}
	// X-OAuth-Scopes is the grant for a classic token, not the permissions accepted by this endpoint.
	// X-Accepted-GitHub-Permissions (and repository permissions for the user) cannot prove the PAT is read-only.
	scopes := strings.TrimSpace(strings.Join(res.Header.Values("X-OAuth-Scopes"), ","))
	if scopes == "" {
		if newReadToken && keychain.FineGrainedGitHubPAT(token) {
			return nil // explicit direct setup from the read-only issuance screen, never an import
		}
		return errors.New("GitHub did not report the token's full grant; create a new fine-grained PAT with only read permissions, then enter it with beacon source add github --new-read-token")
	}
	for _, scope := range strings.Split(scopes, ",") {
		switch strings.TrimSpace(scope) {
		case "read:user", "user:email", "read:org", "read:packages", "read:project", "read:discussion", "read:audit_log":
		default: // repo, public_repo, and anything not known to be read-only
			return errors.New("the GitHub grant is not read-only; create a new fine-grained PAT with only read permissions")
		}
	}
	return nil
}

func (a *app) checkSlackReadGrant(ctx context.Context, token keychain.Secret) (slack.Identity, error) {
	if err := keychain.SlackUserToken(token); err != nil {
		return slack.Identity{}, err
	}
	base := a.slackBase
	if base == "" {
		base = slack.Base
	}
	c := a.grantClient(slack.Hosts(base))
	defer c.CloseIdleConnections()
	id, err := slack.Verify(ctx, c, base, token)
	if err != nil {
		return slack.Identity{}, errors.New("could not verify the user token with Slack; nothing was stored")
	}
	if len(id.Scopes) == 0 {
		return slack.Identity{}, errors.New("the token's scopes were not reported by Slack; issue a new user token with only Beacon's read scopes")
	}
	if len(id.Extra) > 0 {
		return slack.Identity{}, errors.New("the Slack token holds scopes outside Beacon's read-only list; issue a new read-only user token")
	}
	if missing := append(append([]string(nil), id.Missing...), id.MissingNames...); len(missing) > 0 {
		return slack.Identity{}, fmt.Errorf("the token lacks the scopes %s; add them as User Token Scopes, reinstall the app, and run this again. Nothing was stored", strings.Join(missing, ", "))
	}
	return id, nil
}
