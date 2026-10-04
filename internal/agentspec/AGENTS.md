# AgentSpec Runtime Model Package

## OVERVIEW
Normalized, read-only, secret-free view of an agent runtime's options, derived from fragmented upstream sources (config / WS-REST init metadata / workspace override / bot-platform config) by a pure Resolver. Dependency-chain root of the v2 roadmap (#847): identity (#848), runtime events (#849), queue (#851), context (#852), snapshot (#866) all consume AgentSpec fields.

## STRUCTURE
```
agentspec.go      # AgentSpec contract: WorkerSpec / PolicySpec / SandboxSpec / BudgetSpec / IdentityRefs
resolve.go        # Resolver (pure): Input → AgentSpec; precedence + worker-type/sandbox boundary validation
map.go            # MapToStartParams (wired, shadow mode) / MapToSessionInfo (contract-only, NOT wired)
identity.go       # AgentIdentity value object (#848): deterministic AgentID, ctx key plumbing
plan.go           # EffectiveRuntimePlan (#946 spec §6.2): canonical hash identity, Blocked reasons, redacted view
authority.go      # D1 authority ceiling: explicit over-ceiling request blocks, config default clamps
fingerprint.go    # D1 internal LaunchFingerprint + secret-free ConfigPolicyRevision + field-ownership table
snapshot.go       # EffectiveAgentSpecSnapshot (#866): persisted under session context_json, versioned
*_test.go         # 7 test files: identity, map, plan, resolver, snapshot, authority, fingerprint
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| AgentSpec fields | `agentspec.go` | Worker/Policy/Sandbox/Budget/IdentityRefs — which fields each owns |
| Precedence resolution | `resolve.go:77` Resolve | Explicit intent (InitMetadata) → workspace override → config → platform; zero-value = fall through |
| Permission tier | `resolve.go:128` | 4-tier ceiling: read-only \| workspace \| auto-edit \| bypass; empty = worker default |
| Boundary validation | `resolve.go:59` | `validateWorkerType` rejects unknown worker types |
| Entry-layer projection | `map.go` MapToStartParams | Overlays only WorkerType + AllowedTools; idempotent |
| Identity correlation | `identity.go:103` DeriveAgentID | sha1 of (userID, workspaceID, agentName, workerType) — stable across resume |
| Identity ctx plumbing | `identity.go:166/182/197` | WithAgentIdentity / FromAgentIdentity / DropAgentIdentity on session context_json |
| Runtime plan | `plan.go` | PlanVersion=1, PlanResolverID in hash input, ErrPlanBlocked for fail-closed surfaces |
| Permission ceiling | `authority.go` | resolve FIRST, clamp SECOND; explicit request above ceiling blocks, config default clamps with a warning |
| Internal launch identity | `fingerprint.go` | `LaunchFingerprint` ≠ public `PlanHash`; identifies a PLAN, never proves it was applied |
| Field ownership | `fingerprint.go` OwnershipTable | who may decide each field, what clamps it, which rule a future change must not break |
| Snapshot persistence | `snapshot.go` | SnapshotVersion=1, SnapshotContextKey=`_agent_spec`, ErrInvalidSnapshot → fail closed |

## KEY PATTERNS

**Secret-free invariant**: no AgentSpec/AgentIdentity field may carry an API key, env value, or credential — AgentSpec is always safe to log, audit, and emit as event/trace metadata. ConfigEnv / credential-bearing fields stay in the SessionInfo construction stage.

**Pure Resolver**: `Input` collects every upstream fact; Resolver has no store/global dependency → table-driven tests + WS≡REST equivalence proof. Callers must pass already-resolved workspace override via `Input.WorkspacePerm`.

**Mappers overlay, never rewrite**: only AgentSpec-owned fields are projected onto base `worker.SessionStartParams` / `worker.SessionInfo`; everything else passes through untouched. Re-applying a mapper to its own output is a no-op.

**Versioned contracts**: bump `PlanVersion` / `SnapshotVersion` on any breaking shape change so consumers refuse to interpret an unknown plan/snapshot. `PlanResolverID` is part of the canonical plan hash — a new resolver generation yields a different plan identity.

**Fail closed**: `ErrInvalidSnapshot` / `ErrPlanBlocked` results must never be treated as a legacy session with no policy boundary — log redacted diagnostic (shadow) or surface (fail-closed entry paths).

**Authority is asymmetric on purpose**: an explicitly requested tier above the ceiling is BLOCKED, never quietly downgraded (the user asked for something specific); a config-derived tier above it is clamped with a visible warning (config is a guess, the ceiling is the operator's decision). Neither path can produce a plan more permissive than the ceiling.

**Two identities, two jobs**: `PlanHash` is the public, redacted plan identity. `LaunchFingerprint` is the internal identity of the decisions that will actually be applied, and it carries `PlanCoverage` flags so an UNRESOLVED decision is distinguishable from a resolved empty one. Neither may be used as an authorization or approval cache key — only Worker-reported observed facts support that claim.

**Wiring discipline**: behavior-changing integrations enter under shadow mode first. First-cut wires only `MapToStartParams` in the webchat entry path; `MapToSessionInfo` is contract + tests only (SessionInfo is built in the shared bridge layer — wiring is a follow-up slice, design spec §3.5/§9 finding F8).

## ANTI-PATTERNS
- ❌ Add credential/env fields to AgentSpec or AgentIdentity — breaks the secret-free invariant
- ❌ Give the Resolver store/global dependencies — kills the pure-function equivalence proof
- ❌ Derive AgentID from volatile inputs (timestamps, random) — breaks correlation across reconnect/resume
- ❌ Change plan/snapshot shape without bumping the version constant — old binaries misread new data
- ❌ Treat blocked/invalid plans or snapshots as soft warnings — must fail closed
- ❌ Wire mappers into new entry paths directly (REST create, bridge) without shadow mode — behavior change without observation
- ❌ Let `_agent_spec` / `_agent_identity` reserved context keys leak into user-visible session data — system-owned only
