# Console interaction consistency

## Outcome

One selection and editing pattern across Overview, Nodes, Clients, Providers, Routes, and Keys & teams. Select a card, graph node, or table row to inspect it in a right-hand panel. Explicit Edit opens an editor in the same panel. Save changes returns to current details; Cancel returns without writing. Creation uses the same panel and footer. Clients and installation settings retain their read-only meaning.

This follows internal consistency guidance (https://www.nngroup.com/articles/consistency-and-standards/). The panel is nonmodal: background controls remain available, Escape and Close dismiss it, and focus returns to the selected item or a surviving page heading. Do not claim modal semantics without inert background (https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/).

## Implementation boundaries

- Extend the existing `details.mjs` owner with a replaceable content lifetime. Switching details/editor cancels obsolete reads and diagnostics and wipes discarded secrets. Preserve the nested key/team draft through its existing explicit owner, not a general form cache.
- Use one workspace/panel style with a scrollable body and consistent footer. At narrow widths the selected panel occupies the workspace; Close returns to the list/map. Consolidate affected CSS rules instead of appending overrides.
- Reuse `handleSelection` for list views so selecting and closing retain search, pagination, and map position. Category changes explicitly rebuild the page. A separate reload path refreshes authoritative data. Missing/deleted records clear the selection and announce the result. Creation uses a separate URL mode, so a resource named `new` remains selectable.
- All selectable tables use a native name button for keyboard access, clickable rows, and a visible selected state. No duplicate row-level Edit or Manage controls. Page actions consistently place Refresh before the primary + Create/Add action.
- `details.mjs` owns editor dirtiness. Input/change and structural editor actions mark a draft; Cancel, Close, refresh, selection, navigation, sign-out, and browser history consult one page-owned discard guard. Pending writes continue to use `api.canLeave`. Page/auth teardown must still abort and scrub immediately. Browser close uses the standard beforeunload warning for pending writes or dirty drafts. The discard decision precedes navigation/history changes; declining Back/Forward restores the prior history entry without a second prompt.
- Each feature retains its resource reads and mutation rules. Successful writes clear draft state, refresh list data, and read the selected resource before showing details. A readback failure is explicitly a saved-but-unavailable state with a read-only retry, never an editable stale form or a repeated write.
- Preserve partial node updates, provider expected revisions and distinct persistence/activation outcomes, access authority refresh, uncertain-result recovery, route conditional updates, and key disclosure verification. Key creation/rotation remains a one-time disclosure state before returning to details. Nested team save/cancel clears only that editor’s draft; the preserved parent key draft remains dirty and its lifetime stays valid. Closing the panel checks and disposes both.
- Providers gain read-only connection summaries followed by explicit Edit/Connect. Secret presence is displayed, values are never read back. Overview uses the same provider detail/editor owner. Save versus connect remains explicit.
- Clients share the connection guide in the panel. Activity is observational and does not gain a meaningless editor. Settings remains installation guidance; no new backend capabilities are invented.

## Validation and integration

Update interaction tests to select, inspect, and explicitly edit. Preserve existing auth, pending navigation, stale callback, nested draft, secret disclosure, conditional-save, and partial-result tests. Exercise real Chrome at desktop and mobile sizes for selection, Save/Cancel/Close, keyboard, history, selected styling, provider diagnostics, key disclosure, and map stability using synthetic data and actual assets. Run the full project checks and fresh reviews of the entire console arc. Commit only this worktree's console work, integrate main in an isolated clean checkout, and preserve unrelated changes in the main checkout before merging the authorized console work into main.
