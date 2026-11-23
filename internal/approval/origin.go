package approval

import (
	"errors"
	"regexp"
	"strings"
)

const (
	ReviewPort  = "7317"
	LocalHost   = "localhost:" + ReviewPort
	LocalOrigin = "http://" + LocalHost
)

// Origin is the service's one canonical browser origin, not derived from a request or from a proposal.
type Origin struct {
	host  string
	login string
}

var tailnetHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.){2}ts\.net$`)

// ValidOriginURL checks the spelling of an origin independently of its configured Tailscale login.
func ValidOriginURL(origin string) bool {
	host, https := strings.CutPrefix(origin, "https://")
	return origin == LocalOrigin || https && tailnetHost.MatchString(host)
}

func ParseOrigin(origin, tailscaleLogin string) (Origin, error) {
	if !ValidOriginURL(origin) {
		return Origin{}, errors.New("invalid review origin or Tailscale login")
	}
	if origin == LocalOrigin && tailscaleLogin == "" {
		return Origin{host: LocalHost}, nil
	}
	if origin != LocalOrigin && tailscaleLogin != "" && len(tailscaleLogin) <= 254 &&
		strings.IndexFunc(tailscaleLogin, func(r rune) bool { return r < 0x21 || r > 0x7e || r == ',' }) == -1 {
		return Origin{host: strings.TrimPrefix(origin, "https://"), login: tailscaleLogin}, nil
	}
	return Origin{}, errors.New("invalid review origin or Tailscale login")
}

func (o Origin) Host() string           { return o.host }
func (o Origin) TailscaleLogin() string { return o.login }
func (o Origin) Secure() bool           { return o.login != "" }

func (o Origin) URL() string {
	if o.Secure() {
		return "https://" + o.host
	}
	return "http://" + o.host
}
