package spike

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed static/passkey.js static/spike.css
var staticFS embed.FS

var pageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/spike.css">
</head>
<body>
<h1>{{.Title}}</h1>
{{if .Reason}}<p id="reason" class="refused">{{.ReasonText}}</p>{{end}}
{{if .Result}}<p id="result">{{.Result}}</p>{{end}}
{{if .PRFHash}}<p>PRF hash (SHA-256 of the PRF output; the output is not shown):<br><code id="prf-hash" class="hash">{{.PRFHash}}</code></p>{{end}}
{{if .Match}}<p id="match">{{.Match}}</p>{{end}}
{{if .LoginNote}}<p>{{.LoginNote}}</p>{{end}}

<h2>This request</h2>
<table id="headers">
<tr><th>Host</th><td><code>{{.Host}}</code></td></tr>
<tr><th>Origin</th><td><code>{{.OriginHeader}}</code></td></tr>
{{range .Headers}}<tr><th>{{.Name}}</th><td><code>{{.Value}}{{if .Truncated}} (truncated){{end}}</code></td></tr>{{end}}
{{if not .Headers}}<tr><th>Tailscale-*</th><td>(none)</td></tr>{{end}}
</table>

{{if .Recent}}
<h2>Recent requests</h2>
<table>
<tr><th>When</th><th>Method</th><th>Host</th><th>Origin</th><th>Tailscale-User-Login</th><th>Reason</th></tr>
{{range .Recent}}<tr><td>{{.When}}</td><td>{{.Method}} {{.Path}}</td><td><code>{{.Host}}</code></td><td><code>{{.Origin}}</code></td><td><code>{{.Login}}</code></td><td>{{.Reason}}</td></tr>{{end}}
</table>
{{end}}

{{if .Creds}}
<h2>Passkeys for this origin</h2>
<table>
<tr><th>Credential</th><th>Algorithm</th><th>Sign count</th><th>PRF hash</th></tr>
{{range .Creds}}<tr><td><code>{{.ID}}</code></td><td>{{.Alg}}</td><td>{{.Count}}</td><td><code>{{.Hash}}</code></td></tr>{{end}}
</table>
{{end}}

{{if .Ceremony}}
<noscript>Registering and asserting need JavaScript. The passkey API is only reachable from script.</noscript>
<p id="ceremony-error" class="refused"></p>
<form id="ceremony-form" method="post" action="{{.Action}}">
<input type="hidden" name="ceremony" value="{{.CeremonyID}}">
<input type="hidden" name="credential_id" value="">
<input type="hidden" name="client_data_json" value="">
{{if eq .Kind "create"}}
<input type="hidden" name="attestation_object" value="">
<input type="hidden" name="prf_enabled" value="">
<button type="button" id="ceremony-go">Register passkey</button>
{{else}}
<input type="hidden" name="authenticator_data" value="">
<input type="hidden" name="signature" value="">
<input type="hidden" name="prf_output" value="">
<button type="button" id="ceremony-go">Assert</button>
{{end}}
</form>
<script type="application/json" id="ceremony">{{.Ceremony}}</script>
<script src="/passkey.js"></script>
{{end}}

{{if .Nav}}
<p><a class="button" href="/register">Register passkey</a>
<a class="button" href="/assert">Assert</a>
<a class="quiet" href="/register?prf=0">Register without PRF (refused)</a></p>
{{end}}
</body>
</html>
`))

type page struct {
	Title        string
	Reason       string
	ReasonText   string
	Result       string
	PRFHash      string
	Match        string
	LoginNote    string
	Host         string
	OriginHeader string
	Headers      []headerValue
	Recent       []recentRow
	Creds        []credRow
	Ceremony     template.JS
	CeremonyID   string
	Kind         string
	Action       string
	Nav          bool
}

type recentRow struct {
	When, Method, Path, Host, Origin, Login, Reason string
}

type credRow struct {
	ID, Alg, Hash string
	Count         uint32
}

func (s *Server) render(w http.ResponseWriter, status int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTmpl.Execute(w, p); err != nil {
		// The status is already sent. A template error is a programming error;
		// the log line is a fixed code, not the error text.
		s.logResult("page", "template", false, "")
	}
}

var reasonText = map[string]string{
	"host":                          "Refused: Host is not localhost:7317 or a {machine}.{tailnet}.ts.net name.",
	"origin":                        "Refused: Origin is not the canonical origin for this Host.",
	"funnel":                        "Refused: Tailscale-Funnel-Request is set. Funnel traffic is not accepted.",
	"no_tailscale_login":            "Refused: no Tailscale-User-Login. Funnel, without that header, and a tagged device look like this.",
	"tailscale_login_mismatch":      "Refused: Tailscale-User-Login is not the login this spike was started with.",
	"tailscale_header_on_localhost": "Refused: a localhost request must not carry Tailscale-User-Login.",
	"no_prf":                        "Refused: the authenticator did not enable the PRF extension.",
	"no_prf_output":                 "Refused: the assertion had no PRF output.",
	"ceremony":                      "Refused: that ceremony is unknown, already used, or expired.",
	"malformed":                     "Refused: the form could not be read.",
	"credential":                    "Refused: that credential is not registered for this origin.",
	"public_key":                    "Refused: the public key is not ES256, EdDSA or RS256, or is not on its curve.",
	"signature":                     "Refused: the signature did not verify.",
	"sign_count":                    "Refused: a non-zero sign count did not increase.",
	"attestation":                   "Refused: attestation is not none with an empty statement.",
	"client_data":                   "Refused: clientDataJSON could not be read.",
	"type":                          "Refused: clientDataJSON type is wrong.",
	"challenge":                     "Refused: the challenge does not match.",
	"cross_origin":                  "Refused: crossOrigin is true.",
	"rp_id":                         "Refused: the RP ID hash does not match.",
	"auth_data":                     "Refused: authenticator data is not valid.",
	"flags":                         "Refused: authenticator flags are not valid.",
	"user_present":                  "Refused: user presence was not set.",
	"user_verified":                 "Refused: user verification was not set.",
	"full":                          "Refused: this spike already holds its maximum number of passkeys.",
	"exists":                        "Refused: that credential is already registered.",
}

func textFor(code string) string {
	if s, ok := reasonText[code]; ok {
		return s
	}
	return "Refused."
}
