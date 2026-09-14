## Execution report — {{UNIT_ID}}

**Workspace:** `{{WORKSPACE_PATH}}`
**Lab id:** `{{LAB_ID}}` — {{"destroyed, confirmed gone" | "not used" | "not available in this repo yet"}}
**Issues in this dispatch:** {{#<n>, #<n>, … in the order you worked them}}

{{FOR EACH ISSUE — one block per issue in this dispatch, including any you
had to report blocked:}}

### #{{ISSUE_NUMBER}} — {{ISSUE_TITLE}}

**Status:** {{done|blocked}}
**Commit:** `{{SHA}}` {{or "none — blocked"}}

**Files touched:**
- {{path}}

**Summary:** {{2-5 sentences: what was implemented and how}}

**Checks run:** {{every check actually run inside WORKSPACE for this issue,
with its result — and every applicable check that could NOT run on this
machine, with why (not installed, lab not available)}}

**Generated output changed:** {{golden files / api/gen changes and why — or "none"}}

**Destructive code paths touched:** {{for storage/migration/backup work: each
delete, format, sync or relocation path changed, and the test covering it —
or "none"}}

**Design references and defaults:** {{doc sections implemented; any Dn/Qn
this relies on; any doc 13 default you believe is wrong, and why — or "none"}}

**Deviations from the issue text:** {{where and why — or "none"}}

**Left undone / blocked:** {{what and why — or "none"}}

{{END FOR}}

### Findings outside these issues
{{anything real you noticed that none of the issues above cover and you did
not fix: a pre-existing defect, a stale doc reference, a doc 13 default that
looks wrong, a missing make target, tooling missing on this machine. One line
each — what, where (`file:line` or the command that shows it), and why it
isn't yours. The orchestrator files these; you never open an issue. "none" is
fine, but don't drop one because it was inconvenient.}}
