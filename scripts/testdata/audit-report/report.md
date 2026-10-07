---
run_id: 20261007-18c8459
dev_sha: 18c8459000000000000000000000000000000000
scope: [internal/example, area:storage]
units: [example, auth]
date: 2026-10-07
---

## Summary

4 findings: 0 critical, 1 high, 2 medium, 1 low, 0 info; 3 candidates refuted

| id | severity | title | verdict | withhold |
|---|---|---|---|---|
| SA-20261007-18c8459-01 | high | Fixture withheld finding alpha | CONFIRMED | true |
| SA-20261007-18c8459-02 | medium | Fixture public finding: bravo with a colon | CONFIRMED-WITH-PRECONDITIONS | false |
| SA-20261007-18c8459-03 | low | Fixture public finding charlie | CONFIRMED | false |
| SA-20261007-18c8459-04 | medium | Fixture withheld finding delta | CONFIRMED | true |

## SA-20261007-18c8459-01 — Fixture withheld finding alpha

```yaml
id: SA-20261007-18c8459-01
title: "Fixture withheld finding alpha"
severity: high
type: bug
area: area:api
safety_critical: false
withhold: true
verdict: CONFIRMED
files: [internal/example/handler.go, internal/example/store.go]
related: [SA-20261007-18c8459-04]      # [] when none
invariant: T3
cwe: [CWE-22]
```

### Summary

Alpha fixture summary line one. Line two of the summary.

### Entry point and attacker

Doc 15 section 3 entry point; attacker 2.1.

### Verified trace

- `internal/example/handler.go:10` — request arrives
- `internal/example/store.go:42` — value used without a check

```go
// a code sample with a heading-looking line follows
## not a heading
```

### Impact and preconditions

A fixture impact.

### Fix direction

Check the value at the boundary.

### Test to write first

`internal/example/handler_test.go`: assert the boundary check refuses the input.

### Verifier notes

Confirmed by one verifier; severity unchanged.

## SA-20261007-18c8459-02 — Fixture public finding: bravo with a colon

```yaml
id: SA-20261007-18c8459-02
title: "Fixture public finding: bravo with a colon"
severity: medium
type: bug
area: area:storage
safety_critical: true
withhold: false
verdict: CONFIRMED-WITH-PRECONDITIONS
files: [internal/example/pool.go]
related: []      # [] when none
invariant: none
```

### Summary

Bravo fixture summary line one. Line two of the summary.

### Entry point and attacker

Doc 15 section 3 entry point; attacker 2.1.

### Verified trace

- `internal/example/handler.go:10` — request arrives
- `internal/example/store.go:42` — value used without a check

```go
// a code sample with a heading-looking line follows
## not a heading
```

### Impact and preconditions

A fixture impact.

### Fix direction

Check the value at the boundary.

### Test to write first

`internal/example/handler_test.go`: assert the boundary check refuses the input.

### Verifier notes

Confirmed by one verifier; severity unchanged.

## SA-20261007-18c8459-03 — Fixture public finding charlie

```yaml
id: SA-20261007-18c8459-03
title: "Fixture public finding charlie"
severity: low
type: chore
area: none
safety_critical: false
withhold: false
verdict: CONFIRMED
files: [docs/internal/example.md]
related: []      # [] when none
invariant: none
```

### Summary

Charlie fixture summary line one. Line two of the summary.

### Entry point and attacker

Doc 15 section 3 entry point; attacker 2.1.

### Verified trace

- `internal/example/handler.go:10` — request arrives
- `internal/example/store.go:42` — value used without a check

```go
// a code sample with a heading-looking line follows
## not a heading
```

### Impact and preconditions

A fixture impact.

### Fix direction

Check the value at the boundary.

### Test to write first

`internal/example/handler_test.go`: assert the boundary check refuses the input.

### Verifier notes

Confirmed by one verifier; severity unchanged.

## SA-20261007-18c8459-04 — Fixture withheld finding delta

```yaml
id: SA-20261007-18c8459-04
title: "Fixture withheld finding delta"
severity: medium
type: bug
area: area:api
safety_critical: false
withhold: true
verdict: CONFIRMED
files: [internal/example/other.go]
related: [SA-20261007-18c8459-01]      # [] when none
invariant: T3
```

### Summary

Delta fixture summary line one. Line two of the summary.

### Entry point and attacker

Doc 15 section 3 entry point; attacker 2.1.

### Verified trace

- `internal/example/handler.go:10` — request arrives
- `internal/example/store.go:42` — value used without a check

```go
// a code sample with a heading-looking line follows
## not a heading
```

### Impact and preconditions

A fixture impact.

### Fix direction

Check the value at the boundary.

### Test to write first

`internal/example/handler_test.go`: assert the boundary check refuses the input.

### Verifier notes

Confirmed by one verifier; severity unchanged.

## Refuted candidates

- example/C1 — a refuted fixture candidate — REFUTED — the guard at internal/example/guard.go:7 stops it
- example/C2 — a tracked fixture candidate — DUPLICATE #1 — already tracked
- example/C3 — an accepted fixture candidate — ACCEPTED-RESIDUAL 3 — doc 15 section 5

## Coverage

### Units

| unit | files | reviewer outcome | candidates | confirmed |
|---|---|---|---|---|
| example | 3 | reviewed | 4 | 4 |

### Checked and found sound

- example: the boundary checks

### Unassigned files

none

### Notes

none

## Threat-model gaps

none
