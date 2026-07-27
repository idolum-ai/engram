# Pane-scoped GitHub App capabilities

This guide walks through enrolling a GitHub App, requesting a narrowly scoped
installation token from a watched tmux pane, approving that request in
Telegram, verifying the pane lease, and revoking it.

Engram does not turn a GitHub App into ambient shell authority. Each request
names the repositories, permissions, pane, and complete child command that the
user is being asked to approve. Engram gives the minted token only to that child
process and never prints or persists it.

## What you need

Before starting, make sure:

- Engram is configured and running with a private Telegram chat;
- the target terminal is a tmux pane watched by Engram;
- a GitHub App is installed on the user or organization that owns the target
  repositories;
- you know the GitHub App ID and installation ID;
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

Enrollment requires three independent values:

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
capability request verifies that the key, App ID, installation ID, installation
account, repository selection, and requested permissions agree.

Reusing an alias updates that enrollment. It does not silently preserve the
previous unlock mode or key.

## Verify enrollment

Run:

```sh
engram github app list
```

The entry shows:

- alias;
- App ID;
- installation ID;
- `local` or `telegram opt-in` unlock mode; and
- public-key fingerprint.

Confirm the vault remains owner-only:

```sh
stat -f '%Sp %Su %Sg %z %N' "$ENGRAM_HOME/github-apps.json"  # macOS
stat -c '%A %U %G %s %n' "$ENGRAM_HOME/github-apps.json"     # Linux
```

Do not display the vault. Its PEM payload is encrypted, but the file remains
private security material.

The running daemon reloads the vault when a request arrives, so enrollment does
not normally require a service restart.

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
- `@engram_github` advertises the GitHub capability command.

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

The CLI blocks for at most three minutes while Telegram presents the request.

Before approving, verify every field:

- the requesting tmux window;
- App alias;
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

## Inspect the pane lease

In the same pane:

```sh
engram github status
```

The output lists the App alias, remaining lifetime, repositories, and
permissions without exposing the token. The Telegram card gains a compact line
such as:

```text
GH example-app · read-only · 1 repo · 42m
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

## Revoke a pane lease

From the pane that owns the lease:

```sh
engram github revoke
engram github status
```

Engram removes the lease, updates the card, and asks GitHub to revoke the
installation token. Pane loss, replacement, unwatch, expiry, and orderly
service shutdown also remove authority; Engram attempts remote revocation where
a live token still exists.

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

Approvals expire after three minutes. Requester disconnect, cancellation, or
pane invalidation abandons the request. Late Telegram passphrase replies are
consumed and deleted rather than falling through to terminal input.

Removing or replacing an enrolled App while its approval is pending also
cancels the request. Engram repeats that exact enrollment check after token
minting and before delivery. Existing pane leases also bind the App ID,
installation ID, public fingerprint, unlock mode, and enrollment generation;
retargeting an alias cannot reuse the prior installation's token. Start a fresh
request after the intended enrollment is stable.

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
- Approve only the exact pane, App, repositories, permissions, and command
  shown.
- The approved child is trusted with its token and can deliberately disclose
  its own environment.
- Keep the source PEM and encrypted Engram vault owner-only.
- Prefer refusal and a narrower retry over automatic permission escalation.
