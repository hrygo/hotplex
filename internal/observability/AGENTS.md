# Observability Package

## OVERVIEW
Unified OpenTelemetry bootstrap for the gateway: Prometheus metrics endpoint plus OTLP trace/metric export, all behind one `Init()` call with a noop fallback. Owns the instrument registry and the `RegisterGaugeCallbacks` indirection that lets other packages register gauges without racing package `init()`.

## STRUCTURE
```
observability/
  config.go          # Config struct, DefaultConfig(), IsOTELSDKDisabled()
  observability.go   # Init() (sync.Once), Meter()/Tracer(), resource + exporters, gauge registrar
  instruments.go     # lazy instrument accessors, each guarded by its own sync.Once
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Bootstrap | `observability.go:46` Init | `initOnce.Do`; returns shutdown fn |
| Noop fast-path | `observability.go:51` | `OTEL_SDK_DISABLED=true` or resource error → noop meter/tracer, still runs gauge callbacks |
| Meter accessor | `observability.go:90` Meter | Returns `noopMeter` before Init runs |
| Tracer accessor | `observability.go:121` Tracer | Returns `noopTracer` before Init runs |
| Gauge registrar | `observability.go:107` RegisterGaugeCallbacks | Appends to `gaugeCallbackRegistrars`; called by session/pool packages |
| Run gauge callbacks | `observability.go:113` runGaugeCallbacks | Invoked at end of Init only when meter is real |
| W3C propagation | `observability.go:73` | Composite `TraceContext` + `Baggage` |
| Trace exporter | `observability.go:170` initTracing | Noop SDK if no OTLP endpoint; ParentBased TraceIDRatio sampler |
| Metric exporter | `observability.go:207` initMetrics | Prometheus reader always (when enabled) + OTLP periodic reader |
| Prometheus handler | `observability.go:286` MetricsHandler | Returns `promhttp.Handler()`; app code never imports prometheus directly |
| Config defaults | `config.go:71` DefaultConfig | SampleRate 0.1, MetricInterval 30s, CardinalityLimit 5000 |
| Shutdown | `observability.go:128` | Joins tracer + meter shutdown errors via `errors.Join` |

## KEY PATTERNS

**sync.Once Init with noop fallback**
- `initOnce` guarantees single setup. Three noop triggers: `OTEL_SDK_DISABLED=true`, `buildResource` failure, or missing `OTEL_EXPORTER_OTLP_ENDPOINT` (tracing only).
- `Meter()` and `Tracer()` return package-level noop instances whenever `globalMeter`/`globalTracer` are still nil, so callers in early-init code paths never panic.

**RegisterGaugeCallbacks (init-order workaround)**
- Problem: packages like `session` and `pool` own gauge state but their `init()` runs before `observability.Init()`. Calling `Meter().RegisterCallback(...)` from their init hit the noop meter and silently dropped the callback.
- Fix: those packages call `RegisterGaugeCallbacks(fn)` at init time. `Init()` invokes every registered `fn` with the live meter at the end of setup via `runGaugeCallbacks`. Callbacks are skipped under noop (the `globalMeter != noopMeter` guard).

**Per-metric sync.Once**
- Every instrument accessor (`SessionCreated()`, `GatewayMessages()`, ...) holds its own `sync.Once`. First call lazily creates the instrument through `Meter()` and caches it. Repeat calls are free.
- Creation failures are logged via `warnInstrument` (metric name + err) rather than panicked, so a single misconfigured meter cannot take the gateway down.

**Sampler and cardinality caps**
- Root spans sampled at `cfg.SampleRate` (default 10%). Child spans respect parent decisions via `ParentBased`.
- Metric reader enforces `CardinalityLimit` (default 5000) to bound series explosion from high-cardinality labels.

**Core instrument inventory**
- Session: `hotplex.session.created`, `.terminated`, `.deleted`, `.start.attempts`, `.start.errors`, `.start.duration`
- Lifecycle GC: `hotplex.lifecycle.gc.failures`, `.retirement.lag`, `.backlog`, `.oldest_lag`
- Worker: `hotplex.worker.starts`, `.execution.duration`, `.crashes`, `.memory.bytes`, `.creation.duration`
- Gateway: `hotplex.gateway.connections`, `.messages`, `.events`, `.deltas.dropped`, `.platform.dropped`, `.no_subscribers.dropped`, `.delta.coalesced`, `.delta.flush`, `.errors`, `.init.handshake.duration`
- Pool: `hotplex.pool.acquire`, `.release.errors`
- Cron: `hotplex.cron.fires`, `.errors`, `.duration`, `.attached`, `.delivery.result`
- Streaming card: `hotplex.streaming.card.rotations`, `.rotation_failures`, `.flush_fallbacks`
- ACP: `hotplex.acp.prompt_tokens`, `.tool_calls`, `.permission_requests`, `.handshake.duration`
- Session guard: `hotplex.session.transition.guard.repersist.failures`, `.repersist.overwrites`
- Retry: `hotplex.retry.attempts`, `.exhaustion`

## ANTI-PATTERNS
- ❌ Call `Meter().RegisterCallback(...)` from another package's `init()` — returns noop before Init; use `RegisterGaugeCallbacks` instead
- ❌ Skip `errors.Join` in shutdown — partial exporter failures would mask the first error
- ❌ Import `prometheus/client_golang` from app code — go through `MetricsHandler()`
- ❌ Create instruments at package var time — breaks lazy creation and the noop fallback; use the accessor functions
- ❌ Set `SampleRate` above 1.0 or below 0 — `TraceIDRatioBased` expects a ratio in `[0,1]`
