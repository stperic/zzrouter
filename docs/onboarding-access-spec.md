# Installation, onboarding and human access

Status: implemented in the isolated `feat/onboarding-access` worktree, 16 November 2026. Release integration and native installer qualification remain separate.

## Outcome

A person installs zzRouter, opens the console using authority from that installation, creates a local owner account, and gets one model working. People subsequently use username/password sign-in. Applications use named API keys. Computers use the existing pinned enrollment protocol. Teams remain the sole owner of provider, route and budget policy.

Existing installations, API clients, keys, teams, models and worker trust remain valid. No cloud registration, email server or external identity provider is required. The console renders API state and calls API operations; it does not implement access policy.

The initial implementation includes local accounts, invitations, recovery, trusted console entry, resumable onboarding, easier computer enrollment, and coherent release installation. Passkeys, organizational OIDC, delegated team administration and package repositories are later work. Native package generation is included where toolchains support it; actual signing, notarization and clean Windows/Linux OS qualification require the corresponding release environment and credentials.

## User journeys

1. **New personal installation:** install, start, open the console, enter the temporary setup code provided by the authenticated local CLI, create an owner account, choose a provider, prepare a model, test it in Chat. Connecting an app and adding workers are optional follow-ups. Existing browser sessions can be reused. No long-lived administrator key appears in the URL or setup instructions.
2. **Existing installation:** preserve data and credentials, reopen the console, create an owner through the same trusted setup action if no owner exists. Existing key sign-in remains under Advanced access.
3. **Headless installation:** use trusted shell access to obtain a short-lived code and a clean console URL. Open through HTTPS or an SSH-forwarded loopback connection. An arbitrary network visitor cannot claim the installation.
4. **Invited person:** open the invitation, review the router and assigned team, create a username/password account, then use Chat and My access. Application key creation and access management remain administrator-only.
5. **Additional computer, now or later:** open Nodes > Add compute node. Choose its OS, download and verify the matching release, copy the worker installation and pinned connection commands to that computer, then return to approve its temporary pairing details. The node panel leads to provider setup, model download and a test on that exact computer.
6. **Lost owner password:** run a deliberate local recovery command on the installed host. Prove local installation authority, obtain a short-lived recovery code, set a new password, and invalidate old human sessions. Preserve application keys, accounting, providers, models and trust.

## Terminology and console layout

- **People:** human accounts. **Teams:** shared access and budgets. **App keys:** credentials for applications. **Computers:** machines in the existing node topology. **Providers:** local or cloud inference services.
- The sign-in page has one focused panel with router name, username, password and Sign in. Password-manager autocomplete is enabled. Advanced access exposes API-key sign-in and temporary setup/recovery codes. Help explains the local recovery command without exposing secrets.
- Keep the Claude Web default and the user's selected theme. Theme selection moves into signed-in preferences, outside first access. Keep the logo followed by Router.
- People uses the same list, selected-item side panel, edit and create conventions as Routes and Keys & teams. Invitations have pending, accepted, expired and revoked states. Secrets are shown once.
- A compact setup checklist appears until completed or dismissed: owner account, provider, ready model, verified response. App connections and compute nodes remain optional. API state survives browser reload and server restart. Skipping setup does not claim a model is usable.
- After a verified response, show a compact confirmation with Dismiss and, until an application connects, Connect an app. Do not repeat optional node setup or show Skip setup after completion.
- Connect an app reuses named-key creation and displays the selected team's effective access. Ordinary people see administrator instructions until delegated issuance exists.

## Optional compute-node setup

A coordinator with no additional workers is a valid installation. Its first-use checklist finishes after a server-verified response; it never blocks on worker enrollment or an app connection. Nodes > Add compute node remains available after dismissal or completion.

The existing side panel first shows OS selection, a verified-release download page, and copyable install and connection commands supplied by GET `/zzrouter/v1/cluster/pairing/setup`. This administrator-only read contains public coordinator identity, not installation keys. Windows distinguishes a service that starts at boot from a personal installation that lasts for the current session. Its connection command uses a new PowerShell window and the installed PATH, supporting native custom binary directories. Personal and service commands select their respective configuration directories explicitly. Custom configuration overrides require the existing CLI with that installation’s directory.

The cluster domain supplies one canonical HTTPS origin to CLI pairing, setup guidance and enrollment. It uses the cluster listener and advertised/detected addresses independently of the management bind. The worker port follows the coordinator’s cluster bind port, not its public/NAT port. The console may remain on loopback while the cluster port is reachable. Installers treat the two firewall ports separately. Native worker installers collect the cluster port before a fresh install; existing configuration is preserved.

After installing, keep the worker running, paste/upload the temporary pairing document, review its name and fingerprint, then approve it. A definitive pre-delivery refusal lets the user replace the details in the same panel. Once certificate delivery may have committed, the API returns an uncertain outcome and the panel offers inventory inspection instead of repeating enrollment. Pairing success and healthy membership remain separate observations.

Node details show provider/model readiness from GET `/access/onboarding?node=<name>`. A coordinator model cannot satisfy another worker’s readiness. The response milestone remains global and persisted; scoped readiness is current API inventory. Existing provider, discovery, download and Chat screens carry the selected node in the URL through refresh and browser history. Scoped pickers keep it visible even when unavailable. The inference request retains the exact node through the existing API; no screen substitutes the coordinator when worker inventory disappears.

Commands are guidance, not evidence of successful installation or network reachability. Real enrollment, provider observations, downloads and inference establish the successive results. Clean-OS install and reboot tests remain release qualification.

## Identity and authorization

Add a human identity kind to the existing access-control core. Do not represent people as static keys or manufacture a hidden API key per person. Preserve persisted references whose kind is omitted as the legacy static/virtual representation.

A person has an immutable random ID, unique normalized username, display name, Argon2id password hash, role (`admin` or `user`), one shared-team ID, an immutable initial-owner flag, suspension state, credential revision and timestamps. Public responses never contain password hashes or reusable secrets. Roles keep the existing strict meaning. Team owner/member labels do not create new administrative permissions.

Both virtual keys and people are team-bound. All provider, model, route, suspension, concurrency, RPM, TPM and spend checks use this shared predicate. A missing team fails closed. Human accounts cannot escape policy through a static-key branch. The initial owner belongs to an ordinary shared team named Personal workspace; it is not a key-owned personal team, which goes only with its key.

Carry request-time identity kind, identity ID and team ID into usage settlement. Do not infer a person's team by looking up a key after a response. Use a distinct person quota scope where an identity scope is required, plus the existing shared team scope. Changing membership while a request runs must not charge its response to a new team.

Password, suspension, authority and membership changes advance the credential revision. Every session or delegated token re-resolves live identity and team policy. Protect the initial owner from deletion, suspension, demotion or team reassignment through ordinary administrative edits. Protect its shared team from suspension and deletion as well, so a team edit cannot lock out the owner. Provider, route and budget rules still apply to owner inference. Local recovery restores that account without creating a second owner, and refuses with actionable diagnostics if external state corruption has removed its team.

## Storage and mutation boundaries

The people domain owns one versioned, process-locked atomic snapshot containing people, a permanent initialized marker, invitations, one-time grants and onboarding progress. Copy state, validate, persist atomically, then publish in memory. Failed persistence leaves prior state visible. Bounded records and pruning prevent unbounded grant accumulation. Close the lock on shutdown and every failed construction path.

Owner creation, setting initialized, and consuming the setup grant are one people-store transaction. Consuming an invitation and creating its person are likewise one transaction. Restart cannot revive a consumed grant. Never infer uninitialized from an empty account list.

Teams, keys and people live in one access store whose single writer gate serializes every commit. Issuing the owner setup grant creates the owner's workspace team in the same transaction when none is usable, so a failed issue leaves no team. Team deletion checks keys, people and pending invitations in the transaction that deletes, so an invitation issued concurrently either sees the team gone or keeps it. People transitions judge teams against the records of the state they read or commit, never a later or earlier one. Foreign keys from people and keys to teams are a second line of defense.

Human issuer ID/revision is checked directly against the people state of the transaction; it must not call Core to re-enter the same people state. A nonhuman issuer reference may be re-resolved against static/key state without calling OAuth or any human resolver. OAuth can hold its own mutex while resolving people, so no store transaction is open while OAuth runs, and none calls it. Do not introduce an edge between them.

Onboarding PATCH can save a chosen step or dismissal only. Provider/model readiness is computed from the existing API state. A successful-response milestone is recorded by the server's inference settlement path after an actual successful routed response. Neither the browser nor an arbitrary progress PATCH can mark that evidence as verified.

Both GET and PATCH return `provider_ready` and `model_ready` from the existing cached inventory. Match complete models to routable providers by canonical provider key and node. Unpopulated or invalid model caches and unhealthy or unobserved workers do not establish availability. This read never refreshes inventory synchronously. The checklist labels this availability separately from the persisted `response_verified` and `app_connected` milestones.

## Password and session contracts

Use the maintained `golang.org/x/crypto/argon2` implementation with an explicit password hash format, bounded decoding parameters, random salts and constant-time comparison. Initial parameters: Argon2id, 64 MiB, 3 iterations, parallelism 1, 32-byte output. Accept 12 to 128 Unicode characters; do not trim passwords or impose character-class rules. Bound concurrent password computations and request body sizes. Use the same generic invalid-credentials response for unknown and incorrect users. Apply transport-peer throttling through the existing session authority and bounded per-account throttling.

Extend `common/mcp/oauth` with a typed authenticated-callback login entry point. Existing API-key Login delegates to the same implementation. CSRF, exact-origin checks, admission control, rotation, HttpOnly cookies, expiry, persistence and live binding resolution remain in the one existing session authority. Run password or grant verification outside its state mutex; re-resolve the resulting binding before committing a session. Do not add another cookie store or encode structured password/grant data in an API-key string.

An account/grant transaction may succeed before session persistence fails. In that case the account remains valid, the grant remains consumed, and the UI offers normal sign-in. Do not replay enrollment to compensate for a session write failure.

Persist the browser-proof signing key with the shared OAuth state so ordinary restarts preserve both the session and an open editor's proof. Bind proofs to the canonical issuer and opaque session. Reject corrupt persisted keys, and fail startup if initialization cannot persist. Explicit login, logout, revocation and issuer changes retain their existing invalidation behavior. A console 403 probes existing session authority and retains the current editor if that authority still matches; it never retries the refused mutation.

Retain `POST /zzrouter/v1/session` with `{key}`. Add a distinct password endpoint accepting only username/password, plus authenticated password-change and revoke-other-session actions. A successful password change can rotate the current session using the same authority while invalidating old revisions. API-key precedence over cookies remains unchanged.

Application OAuth authorization offers ordinary console account sign-in as well as existing key sign-in. The shared authority supplies a normalized authorization URL from its validated request. The console accepts only the exact relative `/oauth/authorize` endpoint, immediately removes the continuation fragment, and retains it in memory. After successful sign-in it makes a fresh authorization GET because login rotates browser proof. The person still explicitly approves the application; sign-in alone never grants access. Reloading the sign-in page discards the continuation and requires restarting the application connection.

## Trusted console entry

An explicit CLI console-open action requests a grant using its existing authenticated API client. URL discovery, `--print`, copying a URL, and TUI initialization never create grants. First-owner setup requires the static installation administrator credential; an arbitrary virtual administrator cannot claim installation ownership. Existing human or key authority can open only its own corresponding session.

Use a clean URL and a random, short-lived one-time code passed in a same-origin JSON POST. The initial implementation intentionally uses this explicit transfer; it must not automatically approve whichever browser first discovers a public transaction ID. This works for desktop and headless users and avoids designing an unsafe implicit approval ceremony.

The server exchanges the code for a browser-bound grant: a random secret kept only in page memory, bound to the current CSRF/browser proof, the installation, purpose and issuing credential revision. Subsequent owner creation or recovery atomically consumes that bound grant with its account update. Session-entry grants can be consumed directly by the common session login callback. Codes, tokens and passwords are never logged, put in query strings or stored in browser storage.

Setup, sign-in and recovery grants expire after 10 minutes. Invitations default to 24 hours, with an API-enforced upper bound of 7 days. Grants are single-use, hash-stored, revocable, purpose-specific and bounded in count. Revalidate the issuing authority and target team on redemption. Responses use no-store; exact origin, CSRF, protected remote transport and body limits apply even before authentication. A setup status response reveals no account names or installation secrets.

Limit each issuer to 64 active grants. Prune spent non-invitation codes immediately and retain invitation history only through its bounded history window. Reserve capacity for local owner recovery even if ordinary grants fill the store. Reissuing owner recovery replaces the previous recovery grant rather than accumulating more grants.

Invitation URLs may carry a bearer token in the fragment only. Remove the fragment immediately on load before further navigation, retain the token only in memory, and submit through POST. The app loads no third-party resources. State clearly that an invitation does not verify email ownership or provide network reachability.

## Recovery authority

Implement `zzrouter access recover-owner --dir <installation>` as an online local operation. The running daemon creates a restricted local proof file for that installation, rotates it on restart, retains only its expected proof in memory, and removes it when closed. The command reads that file internally and submits it to the local endpoint; it does not print the proof. Reject browser Origin, nonlocal requests, wrong/stale proof and mismatched installation. Loopback alone is not authority. Normal virtual administrator keys cannot replace this proof.

Account recovery belongs to the user-facing `zzrouter` command tree. Move the
existing command and its local HTTP client from `servercli` to `clientcli`, with
their security tests, and remove the node command registration. Do not shell out
to `zzrouter-node` or add a second recovery implementation. Preserve installation
selection, local proof custody, proxy/redirect restrictions, bounded replies and
the existing API. Update the console help and installation guide together; verify
the registered CLI command against a local fixture without changing a real owner
password. The server continues to own grant issuance and password replacement.
The recovery help and code-entry step both name the recovery command. Ordinary
console entry and first-owner setup keep their own instructions while sharing
the same code-exchange form and API.

The proof file is mode 0600 owned by the daemon account on Unix and uses the existing restricted ACL mechanism on Windows. A same-account desktop installation works directly. A system service requires the operator to run recovery with the service account or administrative privileges; a permission error names that requirement without recommending relaxed file permissions. Do not add an operator-readable global recovery secret.

Recovery issues a purpose-bound grant for the immutable owner. On password replacement, advance the revision and invalidate its prior sessions and pending owner grants. Administrator-issued recovery invitations can reset other people, with the same single-use lifecycle. No `config init --force`, data erasure, or offline state editing while a daemon owns the snapshot.

## API surface

All additions live below `/zzrouter/v1`; coordinator only. Administrative operations accept the existing admin gate. Browser mutations also require existing session CSRF/origin controls. Proposed resource names are fixed for implementation:

| Operation | Endpoint |
|---|---|
| Public setup state | `GET /access/setup` |
| Password session | `POST /session/password` |
| Create console grant, authenticated | `POST /access/console-grants` |
| Exchange temporary code | `POST /access/console-grants/exchange` |
| Claim owner with bound grant | `POST /access/setup` |
| List/create invitations | `GET/POST /access/invitations` |
| Revoke invitation | `DELETE /access/invitations/:id` |
| Inspect/redeem invitation | `POST /access/invitations/inspect`, `POST /access/invitations/accept` |
| List/detail/update people | `GET /access/people`, `GET/PATCH /access/people/:id` |
| Issue member recovery invitation | `POST /access/people/:id/recovery` |
| Change own password/revoke other sessions | `POST /access/password`, `POST /access/sessions/revoke` |
| Local owner recovery grant | `POST /access/recovery/local` |
| Redeem recovery | `POST /access/recovery` |
| Onboarding progress | `GET/PATCH /access/onboarding` |

Errors use the existing API error envelope, do not reveal account existence on login, and distinguish validation, expired/used grant, policy refusal, throttling and persistence failure. The normal session state and `/me` projection include identity kind. Versioned optional fields preserve old clients.

## Installation and computer enrollment

Release archives contain all three matching binaries, the platform installer and licenses. One staging recipe serves Makefile and release CI. Installers locate adjacent artifacts and fail before stopping anything if the set is incomplete. Preserve existing node mode, bind address, port, credentials, data and trust. New local installs use loopback by default; enabling remote access is deliberate.

Centralize installation credential creation/reuse in `pkg/config`; stop independent shell/service key generation. Reconcile legacy service credentials according to actual platform consumption. Preserve the credential that the existing daemon uses; if two conflicting authoritative sources cannot be resolved, fail with sanitized source paths instead of silently rotating a key. Preserve unrelated dotenv values. Keep restricted filesystem permissions and use the existing Windows ACL helper. Explicit environment overrides remain explicit overrides.

macOS uses the existing per-user LaunchAgent for Start when I log in. Stop/uninstall unload it rather than triggering KeepAlive to restart. Linux retains systemd; Windows retains the existing service path when elevation is chosen. The lightweight user mode remains available. Package generators must not describe unsigned artifacts as signed. Sign a launcher before computing the embedded launcher integrity hash; sign the final node/client and package afterward.

Linux service installation supports the canonical `/etc/zzrouter` configuration directory. A custom directory remains available in standalone user mode; the installer refuses unsupported system-service overrides before stopping or writing anything. Windows validates the existing service's actual directory and account before changing credential permissions. Its console shortcut pins the selected installation and keeps the temporary-code terminal open after launching the browser.

Computer enrollment adds a versioned, size-bounded JSON transfer document carrying the existing code, worker URL, fingerprint, name and expiry. CLI/TUI export data rather than executable shell snippets. Console and CLI accept the document, show the identity for confirmation and call the existing coordinator-led enrollment API. Keep individual code/address/fingerprint inputs compatible. Discovery never approves trust. Worker expiry and pinned enrollment remain authoritative; no cluster wire protocol bump is required for local human accounts or this transfer format.

## Validation and release acceptance

- Unit/race tests: password parsing and limits, unknown user, concurrent owner claims and invitation redemption, expiry/replay/revocation/restart, failed atomic save, snapshot lock release, team deletion races, stale references, final-owner protection, proof mismatch, and recovery preservation.
- Integration: human and app key share team provider/route/spend/TPM limits; changing membership during a response does not change settlement attribution; legacy reference/session/key behavior remains; worker dispatch stays coordinator-owned.
- Browser: setup, ordinary sign-in, advanced key login, invitation, recovery, People drawers, onboarding resume, mobile/keyboard/password-manager semantics, secret-free URL/storage, clear recoverable write failures.
- Installer: disposable installation roots, missing artifacts, preservation and credential migration fixtures, platform service definitions, pairing transfer malformed/oversize/expired inputs. Build all platform targets. Do not alter this machine's installation for tests.
- Required repo checks: affected `go test -race`, concurrency regressions repeated 20 times, full `go test -race ./...`, `go vet ./...`, `go build ./...`, pinned CI lint, console Node tests, vendor TypeScript checks and browser suite. New common API has its own regression suite and independent review.
- Real signed-package install, reboot, upgrade and recovery on clean macOS/Windows/Linux are release qualification, not inferred from unit tests or cross-compilation. Dependency publication must use an immutable version before the consumer change is merged; no sibling-path replacement is shipped.

Primary guidance: [OWASP password storage](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html), [session management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html), [recovery tokens](https://cheatsheetseries.owasp.org/cheatsheets/Forgot_Password_Cheat_Sheet.html), [Home Assistant onboarding](https://www.home-assistant.io/getting-started/onboarding/), and [Grafana invitations](https://grafana.com/docs/grafana/latest/administration/user-management/manage-org-users/). Research accessed 16 November 2026.

## Workflow completion

Provider credential storage is prepared on first startup by the config owner under
the node account. Explicit key paths remain read-only operator authority. The
browser never provisions or reveals the encryption key.

Team responses include authoritative `membership` counts for people, app keys,
lifecycle-pending invitations and other enrollment grants, plus deletion and
suspension eligibility. Suspended people and suspended/expired keys still count.
Deletion consumes the same dependency projection; shared-key enrollment holds the
existing team mutation gate. The owner team remains protected.

The session response reports `sharing` as `local_only`, `configured` or
`unavailable` from the canonical issuer. `configured` describes HTTPS configuration,
not tested remote access. Invitations, client setup and System use this projection.
MCP browser authorization and API application keys have separate instructions.

Committed human-account changes use the existing audit sink, with trusted actor
and request context. Enrollment is recorded before subsequent session persistence,
so a failed login continuation does not erase its audit event. No-op grant revocation
does not create another event. System Access history reads bounded current-file
pages through an admin API; protocol-level OAuth sign-in/out is outside this coverage.

Recovery belongs to the offline node CLI and stages only into a new directory.
The console shows scope and commands. Daemon-resolved roots avoid service-account
path substitution. The [recovery contract](recovery.md) defines inclusion, key
custody, limits, validation, separate dependencies and activation qualification.
