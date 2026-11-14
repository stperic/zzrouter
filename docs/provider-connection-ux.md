# Provider connection UX

## Review and approach

Reviewed the live Safari provider panel and the existing connection editor on
2026-11-14. The main problems are the extra details step before first setup,
the unchecked activation option, duplicate configuration facts, technical save
receipts presented as the primary feedback, and test errors without a next step.

The documented [LiteLLM setup flow](https://docs.litellm.ai/docs/proxy/docker_quick_start)
and [Open WebUI connection flow](https://docs.openwebui.com/getting-started/quick-start/connect-a-provider/starting-with-openai-compatible/)
both make credential entry, a connection test, and saving explicit. Open WebUI
also documents that a diagnostic can fail even when another provider operation
works. This supports keeping Test separate from Save, rather than making a
successful probe a new prerequisite for saving.

## Architecture plan

- `providers.mjs` owns presentation and action labels. Keep provider fields from
  the connection schema and all validation, persistence and activation in the
  existing REST handlers. No Go, API or protocol changes.
- Open first-time cloud and registry setup directly. Preserve explicit editing
  and independent node observations for local providers.
- Keep Test connection separate from Save. A disabled cloud provider gets a
  primary Save and enable action, with Save without enabling under Advanced
  settings. An enabled provider keeps Save changes without redundant activation.
  An already-configured disabled provider uses the existing provider PATCH to
  enable its saved credentials without a replacement; readback follows that write.
- Reuse the existing panel, draft, session, conditional-write and read-after-write
  mechanisms. A normal save gets a short outcome, with the full durability,
  activation and publication receipt available under Save details.
- Keep saved secrets hidden. A Show/Hide control affects only a newly entered
  value; revealed inputs must still be scrubbed by `clearSecrets`.
- Keep removal and explicit external-credential restoration available under
  Advanced settings. Explain their effects before the existing confirmation.

## Failure paths and validation

Storage unavailable disables writes but allows a diagnostic. Test failures name
the next action and never claim saving occurred. Editing after a test invalidates
that result. Tests do not mutate or activate providers. Writes retain revision
checks, cancellation ownership, stale-completion guards and explicit recovery
after an uncertain write or failed readback. Successful writes, navigation and
session invalidation scrub masked and revealed input values.

Validation: all 175 console Node tests pass, including new setup, activation,
401/403/429 diagnostics, storage-unavailable, revealed-secret and save-receipt
cases. Focused Go console and connection integration tests pass with `-race`;
`go vet ./internal/server` and the release-mode server build also pass.
Browser automation is currently unavailable in this session; the initial review
used a read-only Safari screenshot and accessibility tree.

The production change is +75 lines relative to the deployed base. The increase
adds accessible Show/Hide controls, actionable diagnostic results and enabling
existing credentials; it does not add a backend abstraction or a second save path.
