# Console access redesign

Implement the approved Keys & teams review in the existing console worktree.
Preserve the overview, client key labels and all unrelated changes. Guidance is
read from the main checkout because ignored agent files are absent here.

## Team-owned policy and alignment revision

The user clarified that provider pickers, model permissions, spending caps and
throughput limits belong to teams, not individual keys. Remove the unshipped
key-policy draft and keep the existing team policy as the one provider-access
owner. Do not introduce an intersection or another policy value in keys.

New console keys must choose a shared team, with inline team creation for a
new or dedicated policy. The key form owns name, description, credential role,
team role and expiry only. It shows the chosen team's provider/model policy,
rate limits and shared budget read-only. Editing a team changes all its member
keys. Existing immutable team membership and personal-team lifecycle stay as
supported by the API; do not silently migrate persisted identities or quotas.

Existing API-configured key quotas remain enforced and, when nonzero, are shown
read-only as additional limits. The console does not create new individual
quotas or hide existing restrictions. Personal keys can be inspected, rotated,
suspended or replaced with a key assigned to a policy team. Keep the existing
API quota contracts and ledger; this UI change does not rewrite stored access.

Fix layout at the form owner: consistent control heights, paired input baseline
alignment even when labels wrap, and one aligned action row inside the protected
form. Retain draft, cancellation, pending writes, one-time secret disclosure,
uncertain-outcome recovery, inline team creation and keyboard/narrow layout.

The provider-browser authorization owner must revalidate policy for open
connections as well as new requests. Reuse its existing authentication watcher:
capture the team's provider-policy snapshot at authorization and retire the
session if that policy changes. This also handles sessions opened while policy
allowed all providers, without collecting a separate destination registry.

Validate team inheritance, omitted key quota fields, saved individual-limit
visibility, layout and draft behavior in console tests and Chrome using actual
assets with synthetic data. Test browser connection cancellation after team
policy changes. Run affected Go race tests, vet, build, lint-baseline comparison
and independent reviews. No live configuration changes.

## Ownership and contracts

- `pkg/access/teams` owns a small provider access value: `mode` is `all`,
  `selected` or `none`, and `providers` contains exact provider keys. An omitted
  legacy policy allows all providers. Selected requires a nonempty unique list;
  other modes reject a nonempty list. Invalid persisted policies fail loading.
  Startup refuses non-missing team-file load errors, preventing unrestricted
  operation with a discarded policy. Existing team model allowances remain an additional restriction. Providers
  are cluster-wide identities; this does not introduce per-machine grants.
- Team service, API schemas/OpenAPI, client DTOs, browser identity and audit
  projections carry the same policy. PATCH replaces the complete policy;
  omission retains it. Personal-team immutability and fixed key membership stay.
- The direct resolver filters catalog replicas through an optional provider predicate before choosing a node/provider, preserving one lookup. Existing target admission filters disallowed group candidates before wire
  preference, pricing, fallback and dispatch. Direct and affinity-selected
  providers use the same policy. Default passthrough, response retrieval,
  realtime, native-wire and provider browser routes check their destination.
  Opaque MCP mounts with no known provider refuse restricted teams. Existing
  model/budget restrictions on opaque traffic remain. Anonymous admission
  considers provider-restricted teams too.
- The existing quota engine remains the owner of USD budgets. The console
  presents shared team budgets and existing key restrictions with recorded usage, reservations,
  available amount and UTC reset. No limit means zero in the existing API.
  Period rollover must not present prior-period usage as current. Spending
  estimates are not an invoice guarantee. Per-provider budgets are outside the
  approved review's UI recommendations; no second ledger is introduced.

## Console design

- Searchable Keys/Teams tables show friendly team names, members, access and
  budget summaries. A row action opens the existing right-side detail component.
  Fetch usage only for visible scopes, with explicit loading/unavailable states.
  Selecting a key shows its own and inherited team policy/budget; selecting a
  team shows member keys with direct navigation.
- Key creation starts with name, required team choice and expiry. Generate
  an editable stable ID from the name, preserving API validation/errors. No
  silent unrestricted default: choose a shared team and preview its policy. Create a shared team from this flow and return
  to the retained key draft after success. Keep partial success visible; never
  retry a creation automatically.
- Team access uses All/Selected/None with searchable native checkboxes grouped
  into Local providers and Cloud providers, using existing provider identity
  marks/category metadata. Retain configured but unavailable choices. Optional
  model narrowing uses searchable catalog/group choices plus exact manual entry
  for uncatalogued models, never claiming model names constrain node placement.
- Budget has No limit/Set limit, USD and an explicit calendar period together.
  Throughput and reservation settings move to Advanced. Destructive actions
  remain separate, with shared-team impact visible before saving.
- Reuse current API lifetime, pending-write guard, changed-field conflict check,
  session revalidation, one-time secret handling and rotation recovery. Drawer
  dismissal/navigation aborts reads and clears secrets. No background refresh
  may replace a draft. Keyboard, narrow layout and reduced-motion rules apply.

## Validation

Discriminating Go tests cover policy persistence/validation, legacy behavior,
direct/group/fallback/affinity/opaque admission and changed policy. Existing
schema/client contracts and browser session tests stay green. JS tests cover
provider choices, inherited access, budgets, drafts, partial failure, stale
reads and existing secret/rotation protections. Run affected race tests, vet,
build, console tests and diff checks; compare lint to the established baseline.
Independent architecture and code reviews follow project guidance. Browser
acceptance uses actual assets and synthetic records; no real access changes.

## Progress

- The final team-owned policy revision removes duplicate key permission and
  quota editors. Key creation requires a shared team, supports inline creation
  and previews the inherited rules. Existing individual quotas remain enforced
  and read-only. Control heights, paired inputs and footer actions are aligned.
- Provider-browser authorization snapshots the team policy, including unrestricted
  access, and uses the existing watcher/retirement path to close active sessions
  when that policy changes. Semantic comparison ignores provider ordering.
- Implemented provider access in the existing team domain, resolver, target
  admission and compatibility/opaque routes. API schemas, client DTOs, browser
  identity and creation/update audit events carry the same value.
- Replaced the old form with the searchable workspace and shared detail panel.
  Separate form, provider-policy and budget modules retain the existing mutation
  owner and quota ledger. Inline team creation retains the key draft, including
  late usage reads; stale nested recovery cannot replace a later draft.
- Added shared quota usage projection with explicit tracking state and rollover.
  Startup rejects invalid persisted policies. Provider-restricted discovery
  refuses missing worker provider identity without changing cluster protocol.
- Independent reviews covered architecture, route enforcement, catalog aliases,
  usage, form lifetimes and accessibility. Corrections have focused regression
  coverage. Browser acceptance used actual assets with synthetic records only.
- Final verification and change-size results are recorded in
  `docs/console-network-handoff.md`.
