# Pane-scoped GitHub App capabilities

This guide walks through enrolling a GitHub App, requesting a narrowly scoped
installation token or bounded renewable work-session grant from a watched tmux
pane, approving it in Telegram, inspecting its authority, and revoking it.

Engram does not turn a GitHub App into ambient shell authority. Exact-command
requests name the repositories, permissions, pane, and complete child command.
Renewable grants instead name an explicit pane, App, repository, permission,
purpose, and time envelope. Engram gives minted tokens only to child processes
and never prints or persists them.

## What you need

Before starting, make sure:

- Engram is configured and running with a private Telegram chat;
- the target terminal is a tmux pane watched by Engram;
- a GitHub App is installed on the user or organization that owns the target
  repositories;
- you know the GitHub App ID and every installation ID you intend to enroll;
- you have a private-key PEM for that App; and
- the App installation has only the repository permissions the intended
  commands need.

The read-only examples use GitHub CLI (`gh`) as the approved child command.
Engram itself can broker another executable instead.

GitHub maintains the upstream procedures for
[registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app),
[installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app),
and
[choosing GitHub App permissions](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app).

## Understand the three identifiers

Enrollment requires three independent kinds of value:

1. **App ID** identifies the GitHub App definition. GitHub displays it on the
   App's settings page.
2. **Installation ID** identifies one installation of that App on a user or
   organization. GitHub includes it in the installation settings URL and in
   responses from the
   [installation API](https://docs.github.com/en/rest/apps/apps#get-an-installation-for-the-authenticated-app).
3. **Private-key PEM** lets the App authenticate as itself. It is not an
   installation token and must remain private.

An installation ID is not interchangeable with an App ID, client ID, owner ID,
or repository ID.

The same App definition and private key may serve several installations. Engram
stores those installation IDs under one alias, but it never treats them as one
authority domain: every approval, grant, lease, and token selects exactly one
installation. GitHub installation tokens cannot combine authority across
installations.

Use a short local alias to refer to this enrollment in Engram. The alias need
not equal the public App name, but using a recognizable name makes Telegram
approvals easier to audit.

## Prepare non-secret shell variables

The commands below use shell variables so identifiers do not need to be
repeated. Set them to the real non-secret metadata for the App, installation,
key path, and one test repository:

```sh
export APP_ALIAS='example-app'
export APP_ID='123456'
export INSTALLATION_ID='987654'
export PEM_PATH="$HOME/.config/engram/example-app.private-key.pem"
export OWNER='example-owner'
export REPOSITORY='example-repository'
export ENGRAM_HOME="${ENGRAM_HOME:-$HOME/.engram}"
```

These values are identifiers, not credentials. Do not create a variable for
the PEM contents, vault passphrase, or a minted token.

If the App is installed on more than one account, prepare each installation ID
separately. Do not put a PEM, passphrase, or token in a shell variable.

Shell variables are local to the current shell. Set `APP_ALIAS`, `OWNER`, and
`REPOSITORY` again after moving to a different tmux pane later in this guide.

## Protect the source PEM

Store the PEM in an owner-only directory and set mode `0600`:

```sh
chmod 600 "$PEM_PATH"
```

Inspect metadata without displaying the key:

```sh
stat -f '%Sp %Su %Sg %z %N' "$PEM_PATH"  # macOS
stat -c '%A %U %G %s %n' "$PEM_PATH"     # Linux
```

Do not use `cat`, paste the key into a prompt, add it to a repository, or send
it through Telegram. Enrollment rejects symlinks, files not owned by the
current UID, and files with any group or other permission bits.

## Choose the passphrase route

Engram encrypts a copy of the PEM in
`$ENGRAM_HOME/github-apps.json`. Enrollment prompts twice for a passphrase of at
least 12 bytes. The passphrase is not stored. Use a unique, high-entropy
passphrase rather than a GitHub, operating-system, or API credential.

There are two unlock modes.

### Local unlock: safer default

Without `--telegram-unlock`, `engram github exec` asks for the passphrase in the
local terminal before sending the approval to Telegram. The passphrase never
enters Telegram.

This is the recommended default when someone is present at the machine.

### Telegram unlock: explicit remote opt-in

With `--telegram-unlock`, approving a request causes Engram to send a
forced-reply passphrase prompt in Telegram. Engram deletes the reply and prompt
after processing, and never writes the passphrase to state or audit data.

Telegram bot chats are **not end-to-end encrypted**. The passphrase still
traverses Telegram's cloud and is exposed to anyone controlling the Telegram
account or bot token. Enable this mode only when that tradeoff is understood.

`--local-unlock` overrides a Telegram-enabled enrollment for one execution.

## Enroll the App

First, confirm whether the alias already exists:

```sh
engram github app list
```

For local unlock:

```sh
engram github app add "$APP_ALIAS" \
  --app-id "$APP_ID" \
  --installation-id "$INSTALLATION_ID" \
  --pem "$PEM_PATH"
```

For Telegram unlock:

```sh
engram github app add "$APP_ALIAS" \
  --app-id "$APP_ID" \
  --installation-id "$INSTALLATION_ID" \
  --pem "$PEM_PATH" \
  --telegram-unlock
```

To enroll several installations of the same App and key, repeat the flag in a
single atomic enrollment:

```sh
engram github app add "$APP_ALIAS" \
  --app-id "$APP_ID" \
  --installation-id 987654 \
  --installation-id 987655 \
  --pem "$PEM_PATH"
```

Enter the new vault passphrase twice when prompted. Never place the passphrase
in a command-line argument, environment variable, file, or shell history.

Successful enrollment prints the alias and a public-key fingerprint. The
fingerprint is safe to compare; it is not the private key.

Enrollment:

- reads the source PEM without modifying it;
- validates that it contains a supported private key;
- encrypts a copy with PBKDF2-HMAC-SHA256 and AES-256-GCM;
- stores the encrypted record in Engram's owner-only vault;
- does not save the passphrase; and
- does not mint a GitHub installation token.

Enrollment validates the PEM structure but does not contact GitHub. The first
capability request verifies that the key, App ID, selected installation ID,
installation account, repository selection, and requested permissions agree.

Reusing an alias atomically replaces that enrollment. It does not silently
preserve the previous installation set, unlock mode, or key: repeat every
installation ID that should remain active. The complete set is authenticated
with the encrypted credential so editing vault metadata cannot add an
installation. Existing version-1 single-installation vault entries are read in
place and are not automatically rewritten; explicitly re-enroll the alias to
adopt the multi-installation format.

## Verify enrollment

Run:

```sh
engram github app list
```

The entry shows:

- alias;
- App ID;
- installation IDs;
- `local` or `telegram opt-in` unlock mode; and
- public-key fingerprint.

In `github app list --json`, `installation_ids` is the authoritative set. The
singular `installation_id` field remains present as a backward-compatible
encrypted-record identity anchor; it is not an implicit selector when the set
contains more than one ID.

Confirm the vault remains owner-only:

```sh
stat -f '%Sp %Su %Sg %z %N' "$ENGRAM_HOME/github-apps.json"  # macOS
stat -c '%A %U %G %s %n' "$ENGRAM_HOME/github-apps.json"     # Linux
```

Do not display the vault. Its PEM payload is encrypted, but the file remains
private security material.

The running daemon reloads the vault when a request arrives, so enrollment does
not normally require a service restart.

`engram github app remove "$APP_ALIAS" --yes` removes the whole alias and all
of its installation IDs. To add or remove an individual installation, repeat
`github app add` with the complete desired set, the PEM, and the intended
unlock mode. This deliberate full replacement keeps the installation set and
encrypted key identity atomic.

## Verify the pane-native capability

Move to a tmux pane that already has a live Engram Telegram card. From that
pane, inspect the metadata:

```sh
printf 'TMUX_PANE=%s\n' "$TMUX_PANE"
tmux show-options -pv @engram
tmux show-options -pv @engram_watch_id
tmux show-options -pv @engram_github
```

Continue only when:

- `TMUX_PANE` is nonempty;
- `@engram` contains Engram's versioned capability marker;
- `@engram_watch_id` identifies the current watched session; and
- `@engram_github` advertises both exact execution and renewable-grant command
  shapes.

Empty metadata usually means the pane is not currently watched or the running
Engram version does not include this feature. It can also mean the optional
GitHub credential vault is unavailable; `/status` reports that condition while
the rest of Engram continues running.

## Make the first read-only request

Start with one repository and one read permission:

```sh
engram github exec \
  --app "$APP_ALIAS" \
  --repo "$OWNER/$REPOSITORY" \
  --permission contents=read \
  -- gh api "/repos/$OWNER/$REPOSITORY" \
       --jq '{full_name: .full_name, private: .private, default_branch: .default_branch}'
```

When `github app list` shows multiple installations for the alias, add the
explicit selector:

```sh
engram github exec \
  --app "$APP_ALIAS" \
  --installation-id "$INSTALLATION_ID" \
  --repo "$OWNER/$REPOSITORY" \
  --permission contents=read \
  -- gh repo view "$OWNER/$REPOSITORY"
```

Engram refuses an omitted or unknown selector for a multi-installation alias
before creating a Telegram approval. It never guesses based on repository
names. The selected installation must alone cover every requested repository
and permission; split a cross-installation operation into separate commands.
For selected-repository installations, renewable-grant creation probes the
normalized repository list in deterministic order and stores no authority
unless GitHub confirms that every repository belongs to that exact
installation.

Telegram presents the request for at most fifteen minutes. The CLI and broker
reserve another two minutes for post-approval inspection, minting, validation,
and transactional delivery, so a decision accepted at the deadline does not
race the waiting client.

Before approving, verify every field:

- the requesting tmux window;
- App alias and selected installation ID;
- repository list;
- permission names and levels; and
- complete shell-quoted child command.

Tap **Approve** only when all fields match the intended operation. Tap **Deny**
otherwise.

For Telegram unlock, reply directly to Engram's forced-reply prompt with the
vault passphrase. The prompt and reply should disappear after processing. For
local unlock, the passphrase was collected in the terminal before the approval
message appeared.

On success, the child command prints the requested repository metadata. Engram
does not print the token. It removes ambient `GH_TOKEN` and `GITHUB_TOKEN`
values and supplies the scoped installation token only in the approved child's
environment.

GitHub installation tokens expire after approximately one hour. GitHub's
[installation-token documentation](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app)
describes the upstream lifetime and repository/permission constraints.

## Choose an authority lifetime

Engram exposes three progressively broader, visibly distinct modes:

| Mode | Approval | Bound | Ends |
| --- | --- | --- | --- |
| Exact command | One approval for the full displayed command | Pane, App, repositories, permissions, command | After that request |
| Token lease reuse | No repeat approval for subsets | Same pane, enrollment, repositories, permissions | Upstream token expiry, about one hour |
| Renewable work-session grant | One explicit grant approval | Same pane, enrollment, repositories, permissions, purpose, time | Requested/configured expiry or any invalidation |

A renewable grant reduces interruptions during a known multi-hour workflow. It
does not increase the GitHub App installation's authority, create a permanent
credential, or extend an individual GitHub token.

From the watched pane:

```sh
engram github grant \
  --app "$APP_ALIAS" \
  --repo "$OWNER/$REPOSITORY" \
  --permission actions=read \
  --permission contents=write \
  --permission pull_requests=write \
  --for 6h \
  --purpose "Complete and review the current pull request"
```

For a multi-installation alias, include `--installation-id` in the grant and
repeat the same selector in every grant-backed `github exec`. A grant for one
installation cannot satisfy a request for another.

The approval opens with the facts needed for a quick decision: watched window,
App and repositories, friendly read/write permission groups, duration, absolute
expiry, and purpose. Expand the native **Details** quote before approving when
you need the exact App and installation IDs, fingerprint, tmux binding, raw
permission names, or renewal and in-memory-key semantics. Those diagnostic
facts remain in the same approval message; they are collapsed by default rather
than removed. After a decision, Engram replaces the request with a compact
success or failure summary instead of preserving the full prompt.

For example, a renewable request begins like this:

```text
GitHub access requested · [5] engram

sadasant-ghost → idolum-ai/engram

Write: code, pull requests
Read: actions, checks
For: 2h, renewable · until 2026-08-04 09:36 EDT
Why: Push the review fixes and verify CI

Approve within 15 minutes. The password reply is not end-to-end encrypted.
Details …
```

The requested duration must be at least 30 minutes. Grant expiry is fixed when
the approval request is shown, so this minimum combines the 15-minute approval
window with at least 15 minutes of usable renewable authority even when approval
arrives at the deadline. The instance ceiling defaults to eight hours and can
be lowered (or raised up to the hard 24-hour ceiling) in `.env`:

```dotenv
ENGRAM_GITHUB_GRANT_MAX_DURATION=8h
```

Renewable write authority uses a fail-closed allowlist. Only `checks`,
`contents`, `discussions`, `issues`, `pull_requests`, `repository_projects`,
and `statuses` may be requested at `write`. Evolving permission names remain
eligible at `read`; every other write permission requires an exact `github
exec` approval.

After approval, run ordinary subset commands with `engram github exec`. Engram
mints the first token at the approved grant ceiling and reuses it through its
upstream lifetime, even when concurrent children request different subsets.
This intentionally gives each child the complete ceiling displayed during
approval, preventing a later subset command from revoking a token that an
earlier child is still using. After natural expiry, Engram serializes minting a
replacement at the same ceiling. A request outside the ceiling requires a new
approval.

## Inspect pane authority

In the same pane:

```sh
engram github status
```

The output distinguishes the renewable work-session grant from the current
short-lived token lease, enumerates the repository and permission ceilings,
and reports both expiries without exposing credentials.
The Telegram card shows the broader authority:

```text
GH grant example-app@987654 · 1R 2W · 1 repo · 5h42m
```

Run `engram github status` from another watched pane. It should not see the
first pane's lease.

## Understand lease reuse

A later request can reuse a lease only when all of these remain true:

- it comes from the same immutable tmux server/window/pane binding;
- it names the same enrolled App and public-key fingerprint;
- its repositories are a subset of the lease;
- its permissions are a subset of the lease; and
- the token has not expired.

An equal or narrower request may run without another approval. A broader
repository or permission request requires a new approval.

Request only the authority the command actually needs. Common examples include:

```sh
--permission contents=read
--permission pull_requests=read
--permission pull_requests=write
--permission issues=write
```

The correct permission depends on the GitHub API endpoint. Consult GitHub's
endpoint documentation rather than guessing or automatically escalating after
a refusal.

## Force local unlock for one request

For an enrollment that allows Telegram unlock:

```sh
engram github exec \
  --local-unlock \
  --app "$APP_ALIAS" \
  --repo "$OWNER/$REPOSITORY" \
  --permission contents=read \
  -- gh api "/repos/$OWNER/$REPOSITORY" --jq '.full_name'
```

Enter the passphrase locally. Telegram still presents the exact capability
approval, but no passphrase reply is requested there.

If an existing lease already covers the request, revoke it first to exercise
the complete unlock and approval path.

## Revoke all pane GitHub authority

From the pane that owns the lease:

```sh
engram github revoke
engram github status
```

Engram removes both the grant and token lease, erases the retained in-memory
signing capability, updates the card, and asks GitHub to revoke the installation
token. Pane loss, replacement, unwatch, grant expiry, enrollment mutation or
removal, and orderly service shutdown do the same. A daemon restart never
recovers a prior grant. If GitHub cannot confirm revocation, local authority is
still removed, the CLI reports the failure, and Engram keeps the token only in
an in-memory retry queue. `/status` reports the number of pending revocations
without exposing token values. Engram bounds the combined live, in-flight, and
pending token set at 256 and refuses new token issuance while that budget is
full.

## Remove an enrollment

Revoke active pane leases first, then run:

```sh
engram github app remove "$APP_ALIAS" --yes
engram github app list
```

Removing enrollment prevents new requests. It does not itself revoke a token
already held by a live in-memory pane lease, which is why revocation comes
first.

## Decide what to do with the source PEM

After successful enrollment, Engram uses its encrypted vault copy and no longer
needs the source PEM for ordinary operation. Removing that file may still break
other scripts, services, backup procedures, or token wrappers that read it
directly.

Before moving or deleting a source PEM:

1. verify enrollment and a complete unlock/approval/command cycle;
2. identify every other consumer of the current path;
3. move those consumers to another owner-only key source or retire them;
4. understand that GitHub does not provide the same private key for download
   again; and
5. retain a deliberate recovery path or be prepared to generate and install a
   new GitHub App key.

Engram does not provide a command to export the encrypted PEM back out of its
vault.

## Troubleshooting

### `TMUX_PANE is not set`

Run `exec`, `status`, and `revoke` from inside tmux.

### The pane is not an active Engram session

Use a pane with a current Engram Telegram card and inspect `@engram`,
`@engram_watch_id`, and `@engram_github`.

### The App is not enrolled

Run `engram github app list` and verify the alias exactly. Repeat enrollment if
it is absent.

### Local passphrase entry is required

The enrollment is in local-unlock mode. Enter the passphrase locally, or
intentionally update the enrollment with `--telegram-unlock`.

### The credential could not be unlocked

The passphrase was wrong or the encrypted record could not be authenticated.
No token is returned. Start a fresh request after checking the intended
passphrase.

### The requested permission exceeds the installation

Request a narrower permission or deliberately update the GitHub App
installation. Engram cannot mint authority that the installation does not
have.

### Repository or effective-scope mismatch

Confirm that the installation includes every named repository and permission.
Engram rejects tokens whose effective repository or permission scope differs
from the request. A newly minted token rejected during this validation is
revoked before the error returns.

### Approval expired or the requester exited

Approvals expire after fifteen minutes. The connected CLI and broker keep a
separate two-minute post-approval reserve. Requester disconnect, cancellation, or
pane invalidation abandons the request. Late Telegram passphrase replies are
consumed and deleted rather than falling through to terminal input.

Removing or replacing an enrolled App while its approval is pending also
cancels the request. Engram repeats that exact enrollment check after token
minting and before delivery. Existing pane leases also bind the App ID,
selected installation ID, complete enrolled installation set, public
fingerprint, unlock mode, and enrollment generation;
retargeting an alias cannot reuse the prior installation's token. Start a fresh
request after the intended enrollment is stable.

### The App has several installations

Pass `--installation-id ID` to `github exec` and `github grant`. The error lists
the enrolled non-secret IDs when the selector is absent or unknown. Use
`engram github app list` to inspect the current set, then choose the one whose
account owns every requested repository. Engram intentionally does not probe
all installations and choose on the caller's behalf because that would make
the approved authority identity implicit.

### The repositories span installations

One installation token cannot span GitHub App installations. Request one
installation at a time, splitting repositories into separate `github exec`
commands (and, when longer authority is needed, separate panes/grants). Engram
fails the combined request rather than minting several tokens or widening one.

### The GitHub App vault is unavailable

`/status` reports the vault path and startup error. Engram continues serving its
Telegram and tmux workflows but does not advertise or listen for GitHub
capability requests. Preserve the malformed file for diagnosis, then restore a
known-good owner-only `github-apps.json` or deliberately move it aside and
reenroll the Apps before restarting Engram.

### The command cannot be presented safely

Engram requires the complete shell-quoted command, repositories, and
permissions to fit in one bounded approval message. It also rejects commands
whose contents would be changed by secret redaction. Shorten the command or
move non-secret logic into a reviewed script. Never put secrets in command
arguments.

### The broker is unavailable

Confirm that Engram is running and inspect recent audit records. A healthy
startup records `github.broker` with status `ready`.

## Security reminders

- Never display, paste, commit, or transmit the PEM.
- Never place the vault passphrase in a command, environment variable, or file.
- Telegram unlock crosses a non-E2E cloud boundary.
- Approve only the exact pane, App, selected installation, repositories,
  permissions, and command shown.
- The approved child is trusted with its token and can deliberately disclose
  its own environment.
- Keep the source PEM and encrypted Engram vault owner-only.
- Prefer refusal and a narrower retry over automatic permission escalation.
