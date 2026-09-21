## 🔍 Verification — {{PASS|FAIL}}

**Attempt:** {{N}} of {{MAX}}
**Reviewed commit:** `{{SHA}}` (scratch workspace, not yet landed on `dev`)
{{IF SAFETY_CRITICAL:}}**Safety-critical:** yes — full data-safety review; data-loss test confirmed to fail before and pass after the change{{END IF}}

| Layer | Result (❌ = blocking finding, ⚠️ = notes only) |
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

{{ — only on a fix round, otherwise omit this whole section: }}
### Previous blocking findings
1. **{{short title}}** — {{closed ✅ | still open ❌}} — {{the evidence}}

{{ — only if FAIL, otherwise omit this whole section: }}
### Blocking findings — must be closed before the next attempt
1. **{{short title}}** — `{{file:line}}` — {{what's wrong, the concrete
   scenario that shows it, and what closing it requires}}
2. {{…}}

{{ — only if there are any, otherwise omit this whole section: }}
### Notes — non-blocking, no action required
- {{one line each; never filed as issues, never required of a fix round}}

{{ — only if there are any, otherwise omit this whole section: }}
### Findings outside this issue
Real defects with a concrete scenario, outside this issue's scope — the
orchestrator files or routes each one.
1. **{{short title}}** — `{{file:line}}` or {{the command that shows it}} —
   {{what's wrong, the scenario, and why it isn't in this issue's scope}}

---
*Verified by `task-verifier` via the `orchestrate` skill. This comment does
not close the issue — only a pushed commit's `Fixes #` trailer does that.*
