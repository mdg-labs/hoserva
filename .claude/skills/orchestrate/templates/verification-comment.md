## 🔍 Verification — {{PASS|FAIL}}

**Attempt:** {{N}} of {{MAX}}
**Reviewed commit:** `{{SHA}}` (scratch workspace, not yet landed on `main`)
{{IF SAFETY_CRITICAL:}}**Safety-critical:** yes — full data-safety review; data-loss test confirmed to fail before and pass after the change{{END IF}}

| Layer | Result |
|---|---|
| Correctness / compilation | {{✅ ⚠️ or ❌}} — {{one line}} |
| Scope | {{✅ ⚠️ or ❌}} — {{one line}} |
| Design conformance | {{✅ ⚠️ or ❌}} — {{one line}} |
| Security | {{✅ ⚠️ or ❌}} — {{one line}} |
| Data safety | {{✅ ⚠️ ❌ or ➖ not applicable}} — {{one line}} |
| Best practice / obvious bugs | {{✅ ⚠️ or ❌}} — {{one line}} |

### Checks run
{{the exact commands executed and their outcome — including lab integration
runs with their lab id — and any applicable check that could not run on this
machine, with why}}

{{ — only if FAIL, otherwise omit this whole section: }}
### Findings — must be addressed before the next attempt
1. **{{short title}}** — `{{file:line}}` — {{concrete description of the
   problem and what closing it requires}}
2. {{…}}

{{ — only if there are any, otherwise omit this whole section: }}
### Findings outside this issue
Not blockers for this issue — recorded so they can be filed separately.
1. **{{short title}}** — `{{file:line}}` or {{the command that shows it}} —
   {{what's wrong, and why it isn't in this issue's scope}}

---
*Verified by `task-verifier` via the `orchestrate` skill. This comment does
not close the issue — only a pushed commit's `Fixes #` trailer does that.*
