package spike

import (
	"net/http"
	"regexp"
	"strings"
)

// Port is the only port the spike listens on (13.01-D4). It does not choose another.
const Port = "7317"

// A ts.net name is {machine}.{tailnet}.ts.net: at least two labels before ts.net,
// no port, lowercase. The RP ID is that name.
var tsNetHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+\.ts\.net$`)

// Origin is the canonical origin a request was accepted as.
type Origin struct {
	Mode   string // localhost or ts.net
	Host   string
	Origin string
	RPID   string
}

// classify applies the D4/D14 access checks. A ts.net request without
// Tailscale-User-Login is what Funnel and a tagged device look like, and is
// refused. expectedLogin, when set, is the login recorded for this run; another
// login is refused. An empty expectedLogin still refuses a missing login.
func classify(r *http.Request, expectedLogin string) (Origin, string) {
	host := r.Host
	switch {
	case host == "localhost:"+Port:
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			return Origin{}, "funnel"
		}
		if r.Header.Get("Tailscale-User-Login") != "" {
			return Origin{}, "tailscale_header_on_localhost"
		}
		o := Origin{Mode: "localhost", Host: host, Origin: "http://localhost:" + Port, RPID: "localhost"}
		if reason := checkOrigin(r, o.Origin); reason != "" {
			return Origin{}, reason
		}
		return o, ""
	case tsNetHost.MatchString(host):
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			return Origin{}, "funnel"
		}
		login := r.Header.Get("Tailscale-User-Login")
		if login == "" {
			return Origin{}, "no_tailscale_login"
		}
		if expectedLogin != "" && login != expectedLogin {
			return Origin{}, "tailscale_login_mismatch"
		}
		o := Origin{Mode: "ts.net", Host: host, Origin: "https://" + host, RPID: host}
		if reason := checkOrigin(r, o.Origin); reason != "" {
			return Origin{}, reason
		}
		return o, ""
	default:
		return Origin{}, "host"
	}
}

func checkOrigin(r *http.Request, want string) string {
	got := r.Header.Get("Origin")
	if r.Method == http.MethodPost {
		if got != want {
			return "origin"
		}
		return ""
	}
	if got != "" && got != want {
		return "origin"
	}
	return ""
}

// headerValue is one Tailscale-* header as it arrived, truncated for the log.
type headerValue struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Truncated bool   `json:"truncated,omitempty"`
}

const maxHeaderValue = 512

// tailscaleHeaders returns every Tailscale-* header, names as received.
// Values are truncated so a huge header cannot fill the log. The raw PRF
// output is never a header.
func tailscaleHeaders(r *http.Request) []headerValue {
	var names []string
	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Tailscale-") {
			names = append(names, name)
		}
	}
	sortStrings(names)
	var out []headerValue
	for _, name := range names {
		for _, v := range r.Header.Values(name) {
			truncated := false
			if len(v) > maxHeaderValue {
				v = v[:maxHeaderValue]
				truncated = true
			}
			out = append(out, headerValue{Name: name, Value: v, Truncated: truncated})
		}
	}
	return out
}
