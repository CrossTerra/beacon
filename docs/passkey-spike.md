# Passkey spike

This is the 13.01 part 1 spike, not the service. `beacon spike-passkey` listens on `127.0.0.1:7317` and
`[::1]:7317` only. It registers a passkey with the WebAuthn PRF extension, refuses one without PRF, and asserts
with user verification. The page shows the SHA-256 of the PRF output so the same passkey can be compared across
devices. It never shows or logs the output itself. The spike only lists or accepts a passkey for assertions
after saving its registration.

The spike accepts both `http://localhost:7317` and `https://{machine}.{tailnet}.ts.net` at once, so both origins
can be tried without a restart. A passkey is bound to the origin it was registered on. The later service has one
canonical origin; this spike does not.

It does not read the keychain, does not contact any host, and does not run `tailscale`. Build it with `go build`.
Do not run the installed `beacon`, `scripts/install.sh`, or set `BEACON_TEST_KEYCHAIN`.

Stdout is one JSON object per line. Each request line has `host`, `origin`, and every `Tailscale-*` header that
arrived, under `tailscale`. A result line has `prf` and, after an assertion, `prf_hash`. Nothing in that log is a
request body, an assertion, or a PRF output. The page shows the same headers for the current request and a short
history, so a phone request can be read afterwards on the computer.

## Build

From the Beacon checkout of this commit, on the Mac or on Linux:

```bash
mkdir -p "$HOME/beacon-spike"
go build -o "$HOME/beacon-spike/beacon" ./cmd/beacon
```

## Mac, localhost

```bash
"$HOME/beacon-spike/beacon" spike-passkey --data "$HOME/beacon-spike/data"
```

Leave that running. In Safari (not Chrome: Chrome's passkeys do not sync to iPhone Safari), open
`http://localhost:7317`. Do not use `http://127.0.0.1:7317`; that host is refused.

1. Read the Host and Origin on the page. There must be no `Tailscale-User-*` header. The same line is on stdout.
2. Click **Register passkey**. When macOS asks, save the passkey to iCloud Keychain and use Touch ID. If the
   dialog does not offer iCloud Keychain, cancel. Check Settings → Apple ID → iCloud → Passwords & Keychain, and
   try again. A passkey saved only to this Mac will not appear on the iPhone.
3. The page must say PRF is enabled. If it says the authenticator did not enable PRF, stop. That combination
   cannot be the write-vault key (13.01-D6, D23).
4. Click **Assert**, use Touch ID, and copy the PRF hash into the matrix. Assert again: the hash must be the same,
   and the page says it matches the hash already recorded.

**Register without PRF (refused)** is the negative check. It must be refused and must not add a row to the passkey
table.

## Mac, Tailscale

Tailscale is installed on the Mac and the iPhone, same tailnet, MagicDNS on, HTTPS certificates on. If
`tailscale serve status` already shows a config, copy it down before changing it. `tailscale serve reset` clears
every serve config on this machine, not just this one.

On this Mac the `tailscale` CLI is not on `PATH`. It is
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`. The commands below use that path when the name is not
on `PATH`.

`--tailscale-login` is the person's Tailscale login, the `LoginName` from `tailscale status --json` (an email
address for a personal tailnet), not the display name. A wrong login refuses every ts.net request.

Stop the localhost spike before starting it again. Both modes bind port 7317, and a second process does not
pick another port.

In another terminal:

```bash
/Applications/Tailscale.app/Contents/MacOS/Tailscale status --json
```

Read this machine's DNS name (`Self.DNSName`, without the trailing dot) and your login (`User` → that
`Self.UserID` → `LoginName`). If `Self.Tags` is non-empty, this machine is tagged and has no user login to check.
Stop; do not use it for the ts.net trial.

Stop the spike (Ctrl+C) and start it again with that login, exactly as the header will carry it:

```bash
"$HOME/beacon-spike/beacon" spike-passkey \
  --data "$HOME/beacon-spike/data" \
  --tailscale-login 'you@example.com'
```

Then, with the same Mac binary:

```bash
/Applications/Tailscale.app/Contents/MacOS/Tailscale serve --bg --https=443 http://127.0.0.1:7317
/Applications/Tailscale.app/Contents/MacOS/Tailscale serve status
```

`status` must show port 443 proxying to `127.0.0.1:7317`, and must not show Funnel. If it shows Funnel, turn it
off (`Tailscale funnel --https=443 off`, or `Tailscale serve reset` if that was the only config) and run the serve
command again. `Tailscale` here is that same binary.

In Safari, open `https://{machine}.{tailnet}.ts.net` (no port). This is a different origin from localhost, so
register again, still to iCloud Keychain, still with Touch ID.

1. Record Host, Origin, and every `Tailscale-User-*` header from the page and from stdout. `Tailscale-User-Login`
   must be the login you passed. `Tailscale-Headers-Info` may also be present; record it.
2. Register, confirm PRF is enabled, assert, and record the PRF hash. This hash is the one the iPhone must match.
   It will not match the localhost hash: that was a different passkey.

## iPhone

Tailscale connected to the same tailnet. Safari, not an in-app browser. Open the same
`https://{machine}.{tailnet}.ts.net` link. Do not register a new passkey.

1. Record Host, Origin, and the `Tailscale-User-*` headers. The login should be the same as on the Mac.
2. Tap **Assert** and use Face ID. The synced iCloud passkey should be offered. If Safari wants to create a new
   passkey, the Mac registration was not in iCloud Keychain, or iCloud Keychain is off on the phone. Stop and note
   that; do not register a second one and treat it as the synced passkey.
3. Record the PRF hash. It must equal the Mac's ts.net hash. If PRF is missing, or the hash differs, stop. Do not
   treat PRF as the write-vault key until this matches (13.01-D6, D23).

## Funnel and a tagged device

Both look the same to the spike: a request for the ts.net name with no `Tailscale-User-Login`. Funnel also sets
`Tailscale-Funnel-Request: ?1`. Confirm the refusal from the Mac, against the running spike, without turning
Funnel on:

```bash
curl -sS -D- -o /dev/null \
  -H "Host: {machine}.{tailnet}.ts.net" \
  -H "Tailscale-Funnel-Request: ?1" \
  http://127.0.0.1:7317/
curl -sS -D- -o /dev/null \
  -H "Host: {machine}.{tailnet}.ts.net" \
  http://127.0.0.1:7317/
```

Both must be HTTP 403. The body says why. Stdout has the same reason. A tagged device on the tailnet is the
second request: no user login, no Funnel header. If you have a tagged device, open the https URL from it and
record the refusal. Do not enable Funnel on this machine for the trial; the header above is what Funnel adds.

A localhost request that carries `Tailscale-User-Login` is also refused. The page says so if you send one.

## Linux

On the Linux machine, build the same way and start the spike with your Tailscale login (the login of the Mac and
iPhone, which is who will connect, not a tag):

```bash
mkdir -p "$HOME/beacon-spike"
go build -o "$HOME/beacon-spike/beacon" ./cmd/beacon
"$HOME/beacon-spike/beacon" spike-passkey \
  --data "$HOME/beacon-spike/data" \
  --tailscale-login 'you@example.com'
```

In another terminal, after the same MagicDNS and HTTPS checks, and after noting any existing serve config:

```bash
tailscale serve --bg --https=443 http://127.0.0.1:7317
tailscale serve status
```

From the Mac's Safari, open `https://{linux-machine}.{tailnet}.ts.net`. This is a new origin, so register a new
iCloud Keychain passkey with Touch ID, confirm PRF, assert, and record the hash. From the iPhone, open the same
URL and assert with the synced passkey. The hash must match the Mac's hash for this Linux origin.

Linux on `localhost` is the headless Chrome virtual authenticator, not a security key. From the Beacon checkout:

```bash
go test -count=1 -run TestBrowser ./internal/spike/
```

That registers and asserts with PRF, refuses a credential without PRF, and refuses a wrong origin. Record that row
as covered by the test. By default the test looks for Chrome on `PATH` or at the usual macOS application path.
Set `BEACON_TEST_CHROME` to select a specific executable, as Ubuntu CI does with the runner's preinstalled
`google-chrome`.

## When to stop

If Safari on the Mac, saving to iCloud Keychain, does not get `prf.enabled` on the ts.net origin, or the iPhone
assertion has no PRF output, or the hash differs from the Mac's hash for that same origin: stop. Do not build the
write vault on PRF for that combination. Record the row and leave D6, D9 and D14 for another look. The 2026-11-23
Mac and iPhone run did not hit this stop. Linux passkey testing is deferred (Winton, Nov 23) and is not a
blocker for the next slice.

## Afterwards

Ctrl+C stops the spike. If you changed serve config and had nothing else served, reset it with the same
`tailscale` binary you used (`/Applications/Tailscale.app/Contents/MacOS/Tailscale` on this Mac; `tailscale` on
`PATH` on Linux):

```bash
tailscale serve reset
```

Otherwise put back what `tailscale serve status` showed before. The credential file is
`$HOME/beacon-spike/data/credentials.json` (mode `0600`). It holds public keys, salts and PRF hashes, not the PRF
output. Delete `$HOME/beacon-spike` when you are done with the trial.

## What to record

Headers, one row per request you care about. Recorded 2026-11-23 on Winton's Mac and iPhone. The login header
was checked against `--tailscale-login` and matched. The login itself is not written here. Other Tailscale
header values were not copied into this file. Funnel and tagged-device curls were not part of this run. Linux
passkey testing is deferred (Winton, Nov 23), not a blocker for the next slice.

| From | Host | Origin | Tailscale-User-Login | Tailscale-User-Name | Tailscale-User-Profile-Pic | Other Tailscale-* headers |
| --- | --- | --- | --- | --- | --- | --- |
| Mac Safari, localhost | `localhost:7317` | `http://localhost:7317` | absent | | | none observed |
| Mac Safari, this Mac's ts.net | `wintons-macbook-air.tail6421f3.ts.net` | `https://wintons-macbook-air.tail6421f3.ts.net` | matched `--tailscale-login` | | | not copied |
| iPhone Safari, this Mac's ts.net | `wintons-macbook-air.tail6421f3.ts.net` | `https://wintons-macbook-air.tail6421f3.ts.net` | matched `--tailscale-login` | | | not copied |
| curl, Funnel header | not run | | | | | |
| curl, no login (tagged shape) | not run | | | | | |
| Mac Safari, Linux ts.net | deferred (Winton, Nov 23) | | | | | |
| iPhone Safari, Linux ts.net | deferred (Winton, Nov 23) | | | | | |

Passkeys. The PRF hash is the hex on the page. It is not copied into this file. A localhost hash and a ts.net
hash are different passkeys; only the same origin is comparable. The iPhone hash must equal the Mac hash for
that same ts.net name. This run's Mac and iPhone hashes for the Mac's ts.net origin did match. Two assertions
on localhost gave the same hash. The ts.net assertion hash was stable. That is enough to keep D6: this
combination has PRF, and the synced passkey gives the same output on both devices.

| Browser | Authenticator | Origin | PRF | PRF hash | Notes |
| --- | --- | --- | --- | --- | --- |
| Safari (Mac) | iCloud Keychain, Touch ID | `http://localhost:7317` | enabled | stable; two assertions matched | registered on this origin |
| Safari (Mac) | iCloud Keychain, Touch ID | `https://wintons-macbook-air.tail6421f3.ts.net` | enabled | stable | registered again; `tailscale serve` to `127.0.0.1:7317`, login checked |
| Safari (iPhone) | iCloud Keychain, Face ID | `https://wintons-macbook-air.tail6421f3.ts.net` | enabled | matches the Mac row above | synced passkey, not re-registered |
| Safari (Mac) | iCloud Keychain, Touch ID | `https://{linux}.{tailnet}.ts.net` | | | deferred (Winton, Nov 23); not a blocker |
| Safari (iPhone) | iCloud Keychain, Face ID | `https://{linux}.{tailnet}.ts.net` | | | deferred (Winton, Nov 23); not a blocker |
| Chrome virtual authenticator | virtual, PRF on | `http://localhost:7317` | yes (test) | (test) | `go test -run TestBrowser ./internal/spike/`; covers Linux localhost in CI |
