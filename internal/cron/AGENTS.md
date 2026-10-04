# Cron Scheduler Package

## OVERVIEW
Timer-driven cron scheduler with 3 schedule types (cron expression / every interval / at one-shot), 3 payload types (isolated session / attached session / system event), SQLite persistence, YAML batch import, platform result delivery, exponential backoff retry, and prompt injection validation.

## STRUCTURE
```
cron/
  cron.go        # Scheduler core: in-memory index, CRUD, rebuildIndex, lifecycle
  timer.go       # timerLoop: tick engine, collectDue, CAS concurrency slots, arm/stop
  store.go       # Store interface, SQLiteStore, ErrJobNotFound, scanner pattern
  pg_store.go    # Postgres stub for Store interface
  types.go       # CronJob/CronSchedule/CronPayload/CronJobState + Clone() deep copy
  schedule.go    # NextRun/ValidateSchedule: 3 schedule kinds via robfig/cron/v3 parser
  executor.go    # Executor: starts worker session, sends prompt, waits for completion
  attached.go    # AttachedSessionHandler: dispatch callback into existing session
  delivery.go    # Delivery: extract response + route to platform, in-memory retry queue with exponential backoff
  effect_delivery.go # EffectDelivery interface: gateway-owned delivery contract
  occurrence.go  # Durable per-firing identity (trigger key + generation), content-free
  errors.go      # classifyError: string-based error classification (timeout/network/rate_limit/server/exec), isTemporaryError
  loader.go      # LoadFromYAML: name-idempotent upsert from YAML defs
  retry.go       # backoff schedule, scheduleRetry
  normalize.go   # ValidateJob, ValidateJobPrompt, threat detection, lifecycle constraints
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Scheduler lifecycle | `cron.go` Scheduler struct | Start/Shutdown/CreateJob/UpdateJob/DeleteJob |
| Timer tick engine | `timer.go` timerLoop | arm/stop/tryAcquireSlot (CAS concurrency cap) |
| Concurrency control | `timer.go` tryAcquireSlot | atomic CAS with maxConcurrent limit |
| Job CRUD | `cron.go` CreateJob, UpdateJob, DeleteJob | Validate + persist + rebuild index |
| In-memory index | `cron.go` rebuildIndex/mergeJobState | map[string]*CronJob, reloaded on mutation |
| Store interface | `store.go` Store | Create/Update/Delete/Get/GetByName/List/UpdateState/SetEnabled/UpsertByName |
| SQLite persistence | `store.go` SQLiteStore | WAL mode, writeMu for SQLITE_BUSY prevention |
| Job data model | `types.go` CronJob | ID, Name, Schedule, Payload, State, lifecycle fields |
| Schedule types | `schedule.go` | at (one-shot RFC3339), every (interval ms), cron (5-field expr + tz) |
| Schedule validation | `schedule.go` ValidateSchedule | at: parse RFC3339, every: min 60s, cron: robfig parser |
| Job validation | `normalize.go` ValidateJob | Required fields, schedule, prompt, platform_key, lifecycle constraints |
| Prompt injection guard | `normalize.go` ValidateJobPrompt | 6 threat patterns, 4KB limit |
| Executor | `executor.go` Executor | StartSession → send prompt → poll completion |
| Attached session dispatch | `attached.go` AttachedSessionHandler | ResumeAndInput (idle/terminated) or InjectInput (running) |
| Result delivery | `delivery.go` Delivery | ResponseExtractor + PlatformDeliverer + retry queue + retryLoop |
| Durable occurrence | `occurrence.go` OccurrenceStore | Trigger key + generation is the idempotency boundary; records the delivery owner |
| Delivery owner | `types.go` DeliveryMode + `effect_delivery.go` | legacy_cli (prompt instructs the Agent) vs gateway (effect ledger sends); mutually exclusive, recorded per occurrence |
| YAML batch import | `loader.go` LoadFromYAML | Name-based idempotent upsert, recompute next_run |
| Error classification | `errors.go` | classifyError (timeout/network/rate_limit/server/exec), isTemporaryError |
| Backoff retry | `retry.go` backoff | 30s→1m→5m→15m→1h exponential, scheduleRetry (job retry; see delivery.go for delivery retry) |

## KEY PATTERNS

**Timer tick cycle (timerLoop)**
```
arm(duration) → timer fires → collectDue(now) → for each due job:
  tryAcquireSlot(maxConcurrent) via CAS → executeJob(job) → finishExecution(job)
  → releaseSlot() → arm(nextTickDuration(now))
```

**3 schedule types**
- `at`: one-shot, ISO-8601 timestamp, auto-delete after run (delete_after_run)
- `every`: fixed interval in ms, minimum 60000 (1 minute)
- `cron`: 5-field expression via robfig/cron/v3, optional timezone

**3 payload types**
- `isolated_session`: fresh session per execution (standard)
- `attached_session`: dispatch into existing session (resume or inject)
- `system_event`: reserved, not yet implemented

**CAS concurrency control (timerLoop)**
- `running atomic.Int32` tracks active executions
- `tryAcquireSlot(max)`: CAS loop, returns false if cap reached
- `releaseSlot()`: atomic decrement

**YAML idempotent import (LoadFromYAML)**
- Name as idempotency key: existing → update schedule + recompute next_run, new → create
- Batch import followed by `rebuildIndex()` and `arm(nextTickDuration)`

**Attached session dispatch (AttachedSessionHandler)**
- Running → `InjectInput` (direct stdin write)
- Idle/Terminated → `ResumeAndInput` (re-attach worker + inject)
- Created/Deleted → error (session not ready or gone)

**Result delivery (Delivery)**
- `ResponseExtractor`: extracts last assistant response from completed session
- `PlatformDeliverer`: routes to platform (Slack chat.postMessage / Feishu reply)
- Skipped when: no extractor, empty response, platform is "cron" (self-originated), or silent=true
- **Delivery retry**: in-memory FIFO queue, max 100 entries, max 3 attempts per delivery
  - ⚠️ Queue is in-memory only; entries are lost on process restart
  - Retries transient failures (429, timeout, 5xx) with exponential backoff (30s → 1m, capped 5m; 2m+ unused at maxRetryAttempts=3)
  - Permanent failures (403, 404) logged and discarded immediately
  - Background `retryLoop` goroutine (10s tick) drains due entries via `flushPending`
  - Lifecycle: `StartRetryLoop`/`StopRetryLoop` managed by Scheduler, idempotent
  - Metric: `hotplex.cron.delivery.result{status,platform}` (success/exhausted/permanent)

**Backoff retry (retry.go)**
- Schedule: 30s → 1m → 5m → 15m → 1h (capped)
- Temporary errors: timeout, rate-limit, 5xx, connection refused
- Permanent errors: no retry (invalid config, auth failure)
- `ConsecutiveErrs` counter, `RetryCount` per execution cycle

**Prompt injection guard (normalize.go)**
- 6 threat patterns scanned case-insensitively
- 4KB prompt size limit
- Required platform_key validation per platform (feishu→chat_id, slack→channel_id)

**RunningAtMs sync (finishExecution)**
- `RunningAtMs` set to 0 **before** `UpdateState` call
- Prevents permanent job skip when in-memory and DB state diverge

## ANTI-PATTERNS
- ❌ Set RunningAtMs without syncing to DB — causes permanent job scheduling skip
- ❌ Use `time.Sleep` for scheduling — use timerLoop arm/stop
- ❌ Skip tryAcquireSlot — unlimited concurrency breaks resource budgets
- ❌ Create recurring jobs without max_runs/expires_at — infinite execution risk
- ❌ Skip ValidateJob before persist — invalid schedules/data would corrupt index
- ❌ Access jobs map without Scheduler.mu — race condition with timer goroutine
- ❌ Use `attached_session` with cron expression schedule — only at/every supported
