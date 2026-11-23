# Installing and signing Beacon on your Mac

Beacon is built from your fork on your Mac and signed with your own code-signing identity (plan 13.00-D15). The
signature is how the app's vault, its proposal socket and the collector's keychain items recognise Beacon after a
rebuild. It identifies who signed the build, not which source it came from. Agents and CI never sign or install;
they use `scripts/install.sh --dry-run`, which ad-hoc signs into a temporary directory and touches no keychain.

## Install

From a checkout of the commit you want, with no changes to tracked files, in your own Terminal:

```bash
scripts/install.sh
```

It:

1. builds `Beacon.app` from that commit (`scripts/build-app.sh`, from a Git archive in a private temporary
   directory, with a SHA-256 manifest of the app, the CLI and `tether`). Only the commit reaches the build, so
   untracked and ignored files (a local `.e2e.env`, build outputs) are fine. A change to a tracked file, staged or
   not, stops it: commit or stash it first;
2. picks your code-signing identity: the single valid one in `security find-identity -v -p codesigning`, or the one
   you name with `--identity NAME_OR_SHA1` when there are several;
3. signs the CLI (`dev.beacon.cli`), `tether` (`dev.beacon.tether`) and the app (`dev.beacon.app`). Each gets its
   identifier and a designated requirement pinning that certificate:
   `identifier "dev.beacon.cli" and certificate leaf = H"<SHA-1>"`. `codesign` may ask once to use the identity's
   private key; choose **Always Allow**;
4. verifies each signature against its requirement, before and after installing;
5. quits a running Beacon as its menu's Quit does: services stop and approvals that never started expire. It asks
    through the app's socket with the new CLI (`beacon act quit`, restricted to the bundled CLI's verified external
    peers), then, if it cannot quit through the socket, with SIGTERM. Neither needs permission to control another app.
    It waits up to 30 seconds for each and stops if Beacon is still running. It then replaces
    `~/Applications/Beacon.app`, moving the old copy aside first and back if the move fails;
6. points `~/.local/bin/beacon` at the app's CLI, so `beacon` and `beacon act` on your PATH are the app's. A plain
   file there from an older install is kept as `beacon.pre-app`;
7. removes the old collector LaunchAgent (`dev.beacon.agent`) if it is installed, with `beacon service uninstall`:
   Beacon.app runs the collector now;
8. with a self-signed identity, re-creates the collector's keychain items for this build with
   `beacon keychain repair`, while no collector is running and before the app opens again (see
   [prompts after a rebuild](#keychain-prompts-after-a-rebuild)). It says how many items there are and how many
   prompts to expect, and how to switch to an Apple Development certificate. With an Apple Development certificate
   it skips the repair; `--repair` runs it anyway (once, after changing certificates), and `--no-repair` skips it;
9. opens Beacon (`--no-open` skips this), and prints what it did and the steps Settings still needs: GitHub only
   when no account is set, Signal only when it is missing or names the app as the person.

Run it again after every update: it is idempotent. The first time, in Beacon's menu, open **Settings…** and add a
GitHub token for your account ([which token](approvals.md#merges)), and the Beacon setup code from Signal's `/beacon`
page.

## Choosing an identity

- **Self-signed.** `scripts/dev-signing-setup.sh` creates **Beacon Local Signing** in your login keychain and
  trusts it for code signing for your account. Earlier Beacon builds were signed with it, so it is the natural
  choice if you already have it.
- **Apple Development.** With a free Apple ID, in Xcode choose **Settings → Accounts**, your Personal Team, then
  **Manage Certificates → + → Apple Development**.
  [Apple on Personal Teams](https://developer.apple.com/help/account/basics/about-your-developer-account)

Keep using the same identity. A different certificate is a different signer to everything that pinned the old one.
See [changing the certificate](#changing-the-certificate).

## Keychain prompts after a rebuild

Beacon's keychain items (the collector's credentials and the app's vault key) are created with an access list naming
the creating program's designated requirement: its identifier and your certificate. A rebuild signed with the same
identity meets that requirement.

macOS also keeps a partition list per item. With an **Apple Development** certificate the partition is your team,
which a rebuild keeps, so there is no prompt. With a **self-signed** certificate the partition is the build's own
hash, which every rebuild changes. macOS then asks the first time the new build reads each item. **Always Allow**
asks for your login keychain password, every time. `install.sh` gathers these prompts into the repair it runs
before the app opens: up to two per collector item (to read the item, and to replace it), so a few items mean
several password prompts on every self-signed rebuild. The prompt names the program, so check that it is Beacon.
**Deny** leaves that item unread: the collector reports it, and the app says its vault key is not available. The
app's own vault key asks once more when the app opens.

An **Apple Development** certificate ends this: rebuilds then ask nothing. To get one, in Xcode choose **Settings…
→ Accounts**, click **+**, sign in with your Apple ID (a free one gives a Personal Team), select the team, then
**Manage Certificates… → + → Apple Development**. `security find-identity -v -p codesigning` then lists
"Apple Development: …"; install with `scripts/install.sh --identity "Apple Development: …"`, after the steps in
[changing the certificate](#changing-the-certificate).

The collector's existing items were made by a CLI signed with **Beacon Local Signing** and identifier
`dev.beacon.cli`. The app's CLI keeps both, so the items stay readable; with that self-signed certificate, expect
the one-time prompt per item. [Apple on the file-based keychain](https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains) ·
[the access list](../internal/keychain/darwin.go)

## Changing the certificate

Everything pinned to the old certificate refuses the new one:

- The collector's keychain items ask on every read. Install the new build with `scripts/install.sh --repair` (or run
  `beacon keychain repair` with it) to re-create them with its access.
- The app refuses to open its socket while `~/Library/Application Support/beacon/act.json` names the old signature.
  Remove the file; the next launch writes the new one.
- The vault key asks too, and the vault cannot be read if you deny. Before the vault holds credentials, delete the
  `dev.beacon.app` / `vault-key` item and `~/Library/Application Support/beacon/app/vault`, and relaunch.
- A `service.leaf` left in the state directory by the retired LaunchAgent installer pins the old certificate. See
  [moving from the LaunchAgent](tether.md#moving-from-the-retired-launchagent).

## Optional: check the keychain behaviour yourself

These checks use your login keychain, so only you run them. Do them before the vault holds credentials: the last
two delete the vault.

1. **After install.** The menu shows **Accepting github.merge proposals**, or that no type is enabled yet. In Keychain
   Access, the login item `dev.beacon.app` (account `vault-key`) exists, and its **Access Control** lists only Beacon.
   `act.json` holds the app's designated requirement with your certificate's hash.
2. **Rebuild.** Run `scripts/install.sh` again. With Apple Development there is no prompt. With a self-signed
   certificate there is one prompt for the vault key; choose **Always Allow**. The menu again shows the socket
   accepting proposals.
3. **Ad hoc.** Quit Beacon. Copy the app and ad-hoc sign the copy:
   `ditto ~/Applications/Beacon.app /tmp/AdHoc.app && codesign --force --deep --sign - /tmp/AdHoc.app`.
   Open it: no prompt, and the menu says **Approvals need Beacon.app signed with your Apple Development
   certificate**, with no socket. Quit it and delete the copy.
4. **A key from before the vault.** Quit Beacon and move the vault aside:
   `mv ~/Library/Application\ Support/beacon/app/vault ~/Library/Application\ Support/beacon/app/vault.test`.
   Open Beacon. With no vault, it deletes the existing `vault-key` item and makes a new one, without a prompt.
5. **A planted item.** Quit Beacon. Delete the item and the vault:
   `security delete-generic-password -s dev.beacon.app -a vault-key`
   and `rm ~/Library/Application\ Support/beacon/app/vault`. Plant an item only Terminal's `security` may use:
   `security add-generic-password -s dev.beacon.app -a vault-key -w planted-test-value`. Open Beacon. If macOS asks
   whether Beacon may delete or use the item, choose **Deny**. The menu says **Not opening the vault: a Keychain vault
   key from before the vault could not be removed**. If there is no prompt, Beacon deleted the planted item; check
   that the new item lists only Beacon. Either way, clean up afterwards: delete the item and
   `vault`/`vault.test`, then relaunch.
