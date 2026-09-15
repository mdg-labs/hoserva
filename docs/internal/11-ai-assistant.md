# Hoserva — Built-in AI Assistant

## Why this is a good idea here specifically

Most "AI in the product" features are a chat box bolted onto a UI. This one is different, because the problem it solves is real and specific: **homelab troubleshooting is a needle-in-a-haystack text problem, and the haystack is already on the machine.**

The recurring user experience across every NAS forum is: something is wrong, the evidence is spread across journald, container logs, SMART attributes, a Compose file, and a Samba config, and the user pastes fragments into a forum and waits two days. Hoserva already has all of that data structured and local. An assistant with read access to it can close that loop in seconds.

The spindown problem from doc 08 §1 is the perfect example: "why won't my disks stay asleep" requires correlating wake events, process IO, container configs, and cron schedules. That is exactly the kind of tedious cross-referencing an LLM is good at and humans are bad at.

---

## 1. Principles

**Read-only by default.** The assistant analyses and explains. It can *propose* a change, but applying it always goes through the normal UI flow with the normal confirmation. No "let the AI fix it" button in v1.

**Local-first, and genuinely optional.** The entire product works with the assistant disabled. Off by default; enabling it is a deliberate choice with a clear explanation of where data goes.

**Data leaves the box only on explicit configuration.** BYOK to a cloud provider means logs go to that provider. Say so plainly at the point of configuration, not in a privacy policy.

**Never invent system state.** The assistant works from tool call results, not from recall. If it cannot read something, it says so.

**No secrets in the context.** Redaction happens before the model sees anything, not as a post-filter.

---

## 2. Provider support

### BYOK (cloud)

- Anthropic, OpenAI, Google, OpenRouter, Groq, Mistral — anything with an OpenAI-compatible or natively supported API
- User supplies base URL, API key, and model name. Key stored encrypted in the SQLite DB under the machine key, like every other secret (doc 01 §7, Q28).
- **Generic OpenAI-compatible endpoint** as a first-class option, since it covers most of the long tail

### Local

- **Ollama** — the obvious default for this audience. Auto-detect at `localhost:11434` and on the Docker network.
- **llama.cpp / LM Studio / vLLM / LocalAI** — via the OpenAI-compatible endpoint
- **Ship an Ollama template in the catalog**, with GPU passthrough pre-configured where a GPU is detected, so "install the assistant" is one click

### The abstraction

One internal interface, provider adapters behind it. The assistant code should not know whether it is talking to a frontier model or a 7B running on the same box.

```go
type LLMProvider interface {
    Name() string
    Complete(ctx, Request) (Response, error)
    Stream(ctx, Request) (<-chan Chunk, error)
    SupportsTools() bool
    ContextWindow() int
}
```

**Capability-aware degradation is essential.** A small local model will not reliably drive multi-step tool use. The assistant must detect this and fall back to a simpler mode — single tool call, or pre-assembled context with no tool loop — rather than producing garbage. Concretely:

| Tier | Example | Mode |
|---|---|---|
| Frontier | Claude, GPT-class | Full agentic tool loop, multi-step diagnosis |
| Mid local | 30B+ on a capable GPU | Limited tool loop, 3-4 steps max |
| Small local | 7-8B on CPU | No tool loop. Context is pre-assembled by Hoserva, model summarises and explains |

That last tier matters most for honesty: a 7B model doing free-form diagnosis of a storage array will hallucinate confidently. Constraining it to "here is the relevant log, explain it" keeps it useful and safe.

---

## 3. Tools available to the assistant

Each is read-only, scoped, and redacted. Each tool is a thin wrapper over a documented read operation in `api/openapi.yaml` (D18), called with viewer authorization — the assistant has no private access to the system and sees nothing a viewer-role API token couldn't, before its own redaction on top.

| Tool | Returns |
|---|---|
| `get_system_status` | Array health, parity freshness, disk states, capacity |
| `get_disk_details` | SMART attributes and history for a disk |
| `read_journal` | journald, filtered by unit/priority/time window |
| `read_container_logs` | Logs for a named container, tail-limited |
| `get_container_config` | The Compose file and resolved config for a stack |
| `list_containers` | All containers, state, image, ports, mounts |
| `read_generated_config` | snapraid.conf, smb.conf, exports, mount units |
| `get_job_history` | Recent jobs, results, failure output |
| `get_parity_diff` | The current SnapRAID diff |
| `get_spindown_events` | Wake events with attribution (doc 08 §1) |
| `get_share_config` | Share definitions and permissions |
| `search_docs` | The Hoserva documentation site, from a search index shipped inside the `.deb` — works offline, never fetches the live site |

### Explicitly not available

- Writing any file
- Executing arbitrary commands
- Reading user data in the pool (the assistant sees *about* files, never their contents)
- Reading secrets, API keys, passwords, certificates — redacted before the tool returns

### Redaction

Applied at the tool boundary, not the prompt boundary:

- Known secret fields from the DB schema: never included
- Environment variables matching `*PASSWORD*`, `*TOKEN*`, `*KEY*`, `*SECRET*`: values replaced with `[redacted]`
- Log lines matching credential patterns: redacted
- Public IPs and hostnames: optionally redacted, user-configurable
- **The user can preview exactly what would be sent** before enabling a cloud provider — one screen showing a sample payload. This turns an abstract privacy question into a concrete one.

---

## 4. Where it appears in the UI

Not a floating chat bubble. Contextual entry points where a question actually arises:

- **Failed job** → *Explain this failure* on the job detail page
- **Container crash-looping** → *Diagnose* on the container card
- **SMART warning** → *What does this mean?* on the disk detail page
- **Sync blocked by threshold** → *Review these deletions* — summarise what was deleted and whether the pattern looks like cleanup or like something going wrong
- **Disks not sleeping** → *Why are my disks awake?*, running against the spindown attribution data
- **Compose preview in the install wizard** → *Review this configuration* — flag privilege requests, odd paths, missing volumes
- **Migration scan report** → *Explain this report* in plain language
- **Free-form chat** at `/tools/assistant`, for everything else

Each contextual entry point pre-loads the relevant context. The user does not have to know which logs matter — that is the entire value.

---

## 5. Cost, context, and honesty

**Token discipline.** Logs are enormous. Never dump raw. Pre-filter by time window and severity, deduplicate repeated lines with counts, truncate with explicit markers. A 50 MB journal becomes 3 KB of relevant lines.

**Show the cost.** For BYOK cloud providers, display estimated tokens before sending and actual usage after. Users on metered APIs need this, and nobody else shows it.

**Show the context.** An expandable "what the assistant looked at" panel listing every tool call and what it returned. This is both a trust mechanism and a debugging aid — when the answer is wrong, the user can see why.

**Never silently fall back.** If the configured provider is unreachable, say so. Do not quietly switch to a different model.

---

## 6. Safety boundaries

The assistant is advising on a system where bad advice destroys data. Hard rules:

- **Never suggest running `snapraid sync` in response to a blocked sync** without explicitly explaining that doing so destroys the parity that could recover the deleted files. This is the single most dangerous piece of advice possible in this product and it must be handled with an explicit guardrail in the system prompt *and* a UI-level interception of that suggestion.
- **Never suggest formatting, `mkfs`, `xfs_repair -L`, or `snapraid fix --force`** as a first step. Where such a command is genuinely the answer, present it with the consequence stated and a pointer to the documented procedure.
- **Distinguish what it read from what it inferred.** "Your log shows X" and "this usually means Y" are different claims and must be phrased differently.
- **Defer on data-loss decisions.** When the correct action risks data, the assistant explains the options and stops. It does not recommend.

These go in the system prompt *and* in a response post-filter that flags destructive commands, because a small local model will not reliably honour prompt instructions.

---

## 7. Implementation notes

- Lives in `internal/assistant/`, behind a feature flag, with zero impact when disabled
- Tools call the API through the generated Go client, never internal packages directly (D18)
- Conversations stored in SQLite with a retention setting, default 30 days, and a clear-all action
- Streaming responses over the existing SSE channel
- **Tool results are cached briefly** so a multi-step diagnosis doesn't re-query SMART five times
- The system prompt is a versioned file in the repo, reviewable in PRs, not a string buried in code
- Evaluated against a fixture set of real failure scenarios — a corrupted disk, a crash-looping container, a blocked sync — asserting the assistant reaches the right diagnosis. This is testable and should be tested.

---

## 8. Phasing

**Post-1.0** (Q47). The storage core has to be trustworthy before anything else matters, and an assistant on an unfinished product diagnoses its own bugs. (Earlier drafts said both "not v1" and "Phase 3 or 4" — but 1.0 ships at the end of Phase 4, so those contradicted each other.) The one exception: step 1 below may land in Phase 4 if, and only if, everything else in Phase 4 is done; it is the first thing cut otherwise.

Suggested order:

1. Provider abstraction + local Ollama + free-form chat with `search_docs` only — useful immediately, minimal risk
2. Read-only system tools, contextual entry points
3. Spindown attribution analysis — the differentiating use case
4. Proposed-change flow (assistant drafts a config change, user applies through the normal UI)

Step 1 alone is worth shipping: "ask questions about your own NAS, answered from the actual docs, running entirely locally" is a genuinely useful feature with almost no blast radius.
