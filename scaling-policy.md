# Pluggable Scaling Policy for FrankenPHP

## Problem

FrankenPHP's CPU probe (`ProbeCPUs`) sleeps for 120ms on every scale-up attempt to
measure CPU usage via `clock_gettime` delta. Combined with the 5ms `minStallTime` and
0.8 CPU threshold, this creates a ~125ms gate before any thread is added. That's the
right tradeoff for HTTP — it prevents thrashing on short-lived request bursts.

But the behavior is hardcoded, and not all workloads are HTTP.

For internal parallelism (subrequests, async task execution), every millisecond of stall
delay adds directly to request latency. A 125ms gate on each scale-up means a fan-out of
10 subrequests pays over a second just waiting for threads. That's not acceptable.

The current scaling constants are baked into `scaling.go`:

```go
minStallTime          = 5ms
cpuProbeTime          = 120ms   // blocking sleep in ProbeCPUs()
maxCpuUsageForScaling = 0.8
downScaleCheckTime    = 5s
maxTerminationCount   = 10
defaultMaxIdleTime    = 5s
```

The CPU check (`ProbeCPUs`) also conflates measurement with decision — it sleeps, probes,
AND decides in one call, so there's no way to change the threshold or remove the delay
without forking the function.

Related: [#2306](https://github.com/php/frankenphp/pull/2306)

## Solution

Keep the current behavior as the default. Make it pluggable.

Introduce a `ScalingPolicy` interface that receives metrics and returns a boolean.
The scaling infrastructure collects the data, the policy decides what to do with it.
The default policy preserves current behavior exactly. Alternative policies can
remove the delay, raise the CPU threshold, or make decisions based on request type.

A `RequestType` is added to identify what kind of request triggered the scaling demand.
This allows a single policy to understand the context — an HTTP request and an internal
subrequest have fundamentally different latency requirements, and the policy can act
accordingly.

### Interface

```go
type ScalingPolicy interface {
    ShouldScaleUp(metrics ScalingMetrics) bool
    ShouldScaleDown(metrics ScalingMetrics) bool
}

type ScalingMetrics struct {
    CPUUsage      float64       // fraction of total CPU capacity (0.0–1.0)
    StallDuration time.Duration // how long the request has been waiting
    IdleDuration  time.Duration // how long the thread has been idle
    ActiveThreads int           // currently autoscaled threads
    MaxThreads    int           // autoscale ceiling
    RequestType   RequestType   // see RequestTypeDefault, RequestTypeAsync
}
```

### CPU measurement

`probeCPULoad(maxLoadFactor)` is replaced by `getCPUUsage() float64`:

```go
// Before: probes AND decides, returns bool
func probeCPULoad(maxLoadFactor float64) bool {
    cpuPercent, _ := cpuProc.Percent(0)
    return cpuPercent < float64(cpuCount) * 100.0 * maxLoadFactor
}

// After: probes only, returns normalized value
func getCPUUsage() float64 {
    cpuPercent, _ := cpuProc.Percent(0)
    return cpuPercent / (float64(cpuCount) * 100.0)
}
```

Returns 1.0 on error (blocks scale-up as safe default).

`getCPUUsage()` is called on both the scale-up path (per stalled request) and the
scale-down path (every `downScaleCheckTime` cycle). Calling it in the downscale loop
keeps the `Percent(0)` measurement window tight — so the next scale-up always has a
fresh baseline — and gives `ShouldScaleDown` CPU data for smarter deactivation decisions
(e.g. keep threads warm while CPU is high).

### Built-in policies

**DefaultScalingPolicy** — equivalent to current behavior, zero behavioral change:

```go
func NewDefaultScalingPolicy() *DefaultScalingPolicy {
    return &DefaultScalingPolicy{
        MinStallTime: 5 * time.Millisecond,
        MaxCPUUsage:  0.8,
        MaxIdleTime:  5 * time.Second,
    }
}

func (p *DefaultScalingPolicy) ShouldScaleUp(m ScalingMetrics) bool {
    return m.StallDuration >= p.MinStallTime && m.CPUUsage < p.MaxCPUUsage
}

func (p *DefaultScalingPolicy) ShouldScaleDown(m ScalingMetrics) bool {
    return m.IdleDuration > p.MaxIdleTime
}
```

**ImmediateScalingPolicy** — no stall delay, higher CPU threshold:

```go
func NewImmediateScalingPolicy() *ImmediateScalingPolicy {
    return &ImmediateScalingPolicy{
        MaxCPUUsage: 0.95,
        MaxIdleTime: 5 * time.Second,
    }
}

func (p *ImmediateScalingPolicy) ShouldScaleUp(m ScalingMetrics) bool {
    return m.CPUUsage < p.MaxCPUUsage
}

func (p *ImmediateScalingPolicy) ShouldScaleDown(m ScalingMetrics) bool {
    return m.IdleDuration > p.MaxIdleTime
}
```

### Request typing

```go
type RequestType string

const (
    RequestTypeDefault RequestType = ""      // untagged, standard HTTP
    RequestTypeAsync   RequestType = "async" // internal subrequests
)

func WithRequestType(rt RequestType) RequestOption
```

The request type flows through the scaling path:

1. Request created with `WithRequestType(RequestTypeAsync)`
2. Request stalls, `frankenPHPContext` sent to `scaleChan`
3. `startUpscalingThreads` reads `fc.requestType` into `ScalingMetrics.RequestType`
4. Policy uses it to differentiate behavior
5. Autoscaled thread inherits the type (`phpThread.requestType`)
6. `deactivateThreads` passes thread's type into `ShouldScaleDown`
7. Type reset to `RequestTypeDefault` when thread is deactivated

The type is generic — not scaling-specific. Other subsystems (metrics, logging) can
read it from the context. Users can define their own types (`RequestType("batch")`).

Built-in policies ignore the request type. Custom policies can use it for mixed
workloads — and compose with the built-in policies via embedding rather than chaining:

```go
// Custom policy that overrides behavior for async requests
// and delegates everything else to the default.
type AsyncPolicy struct {
    Default *DefaultScalingPolicy
}

func (p *AsyncPolicy) ShouldScaleUp(m ScalingMetrics) bool {
    if m.RequestType == RequestTypeAsync {
        return m.CPUUsage < 0.95 // instant, no stall delay
    }
    return p.Default.ShouldScaleUp(m) // fall through to default
}

func (p *AsyncPolicy) ShouldScaleDown(m ScalingMetrics) bool {
    if m.RequestType == RequestTypeAsync {
        return m.IdleDuration > 2*time.Second
    }
    return p.Default.ShouldScaleDown(m)
}

// Usage:
frankenphp.Init(
    frankenphp.WithScalingPolicy(&AsyncPolicy{
        Default: frankenphp.NewDefaultScalingPolicy(),
    }),
)
```

This is a deliberate design choice: composition over chaining. There is no built-in policy
chaining or fallback mechanism. A custom policy embeds the default and delegates to it —
standard Go composition. This keeps the interface at two methods, avoids ordering semantics
and "not handled" signaling, and lets the policy author control exactly what falls through.

### Option

```go
func WithScalingPolicy(policy ScalingPolicy) Option
```

If not set: `DefaultScalingPolicy`. `WithMaxIdleTime` still works — configures the default
policy's `MaxIdleTime`. Explicit `WithScalingPolicy` takes precedence.


## Usage: FrankenAsync

```go
// main.go
frankenphp.Init(
    frankenphp.WithNumThreads(numThreads),
    frankenphp.WithMaxThreads(maxThreads),
    frankenphp.WithScalingPolicy(frankenphp.NewImmediateScalingPolicy()),
)

// phpext/phpext.go — subrequests tagged for policy differentiation
req, _ := frankenphp.NewRequestWithContext(r,
    frankenphp.WithRequestType(frankenphp.RequestTypeAsync),
    frankenphp.WithRequestEnv(envCGI),
    frankenphp.WithOriginalRequest(origReq),
)
```

## References

This pattern is well-established in other autoscaling systems.

### Comparison

| | FrankenPHP | [K8s HPA v2](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#configurable-scaling-behavior) | [Java TPE](https://docs.oracle.com/en/java/javase/17/docs/api/java.base/java/util/concurrent/ThreadPoolExecutor.html) | [KEDA](https://keda.sh/docs/2.19/concepts/) | [Envoy](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking) |
|---|---|---|---|---|---|
| Pluggable policy | `ScalingPolicy` interface | `behavior` field | Implicit via queue type | Scalers + triggers | Per-priority thresholds |
| Scale-up/down separation | `ShouldScaleUp`/`Down` | `scaleUp`/`scaleDown` | Keep-alive only | Activation vs scaling | Separate limits |
| Measurement/decision split | `getCPUUsage` → policy | Metrics server → HPA | No | Scaler → metrics server → HPA | No |
| Per-request-type behavior | `RequestType` | No (per-deployment) | No | No | Yes (priority classes) |
| Stabilization window | `MinStallTime` | 5min default (configurable) | No | `cooldownPeriod` | No |
| Scale-up rate limiting | No (intentional) | Max pods per period | No | No | No |
| Scale-down rate limiting | `maxTerminationCount` | Max pods per period | No | No | No |
| Scale to zero | No | No | `allowCoreThreadTimeOut` | Yes | N/A |
| Type inheritance (up→down) | Yes | No | No | No | No |
| Rejection policy | `maxWaitTime` timeout | N/A | Pluggable (4 built-in) | N/A | Circuit breaker overflow |

### Analysis

**Kubernetes HPA v2** went through the same evolution: v1 had hardcoded scaling behavior,
v2 added a [`behavior`](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#configurable-scaling-behavior)
field with separate `scaleUp`/`scaleDown` policies, stabilization windows, and rate limits.
The motivation was identical — different workloads need different scaling characteristics.
Our `ScalingPolicy` interface maps directly: `ShouldScaleUp`/`ShouldScaleDown` with
`MinStallTime` as the stabilization window. See the
[KEP](https://github.com/kubernetes/enhancements/blob/master/keps/sig-autoscaling/853-configurable-hpa-scale-velocity/README.md)
for the design rationale.

**Java's ThreadPoolExecutor** encodes policy implicitly through queue type:
`SynchronousQueue` hands off tasks directly to threads — if no thread is available a new one
is created immediately (our `ImmediateScalingPolicy`). `LinkedBlockingQueue` buffers tasks and only
scales to `corePoolSize` (our `DefaultScalingPolicy` with stall delay). Same two modes, but the
policy is baked into the queue choice — you can't switch behavior based on task type at runtime.

**KEDA** (Kubernetes Event-Driven Autoscaler) has the cleanest separation of concerns:
_scalers_ are metric providers that connect to external sources, _triggers_ define when to
activate, and the _metrics server_ feeds data to the HPA which makes the actual scaling
decision. Our `getCPUUsage()` → `ScalingMetrics` → `ScalingPolicy` follows the same
layering: measurement, data, decision.

**Envoy** applies different circuit breaker thresholds per routing priority — one connection
pool, different limits per traffic class. Our `RequestType`-based differentiation follows the
same idea: one thread pool, different scaling strategies per request type.

### What we deliberately don't do

Features present in cluster-level autoscalers that don't apply to in-process thread scaling:

- **Long stabilization windows** (K8s: 5 min default) — prevents flapping when pods take
  minutes to start. Threads activate in milliseconds and cost ~350KB idle. Flapping is cheap.
  `MinStallTime` is sufficient.
- **Scale-up rate limiting** (K8s: max N pods per period) — prevents runaway pod creation.
  We scale one thread per channel read, naturally rate-limited. `ImmediateScalingPolicy` explicitly
  wants maximum scale-up speed.
- **Scale to zero** (KEDA) — reclaims idle resources completely. The cold-start latency of
  booting a PHP thread from zero would hurt more than ~350KB of memory saves.
- **Composable scalers** (KEDA: multiple metrics sources) — overkill for thread pools. One
  policy with `RequestType` differentiation covers our cases.

### What this adds

Request type inheritance from request → thread → deactivation. Most autoscalers don't track
_why_ a resource was scaled up when deciding whether to scale it down. This lets policies
make smarter deactivation decisions in mixed workloads — e.g. reclaim async-spawned threads
faster than HTTP-scaled threads.

### Links

- [#2306](https://github.com/php/frankenphp/pull/2306) — cross-platform CPU probe
- [#2287](https://github.com/php/frankenphp/pull/2287) — background workers scaling discussion
- [FrankenAsync](https://github.com/johanjanssens/frankenasync) — async subrequests, demonstrates the need
- [Discussion #2223](https://github.com/php/frankenphp/discussions/2223) — PHP extensions with FrankenPHP
- [Kubernetes HPA v2 — Configurable Scaling Behavior](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#configurable-scaling-behavior)
- [KEP-853 — Configurable HPA Scale Velocity](https://github.com/kubernetes/enhancements/blob/master/keps/sig-autoscaling/853-configurable-hpa-scale-velocity/README.md)
- [Java ThreadPoolExecutor](https://docs.oracle.com/en/java/javase/17/docs/api/java.base/java/util/concurrent/ThreadPoolExecutor.html)
- [KEDA Concepts](https://keda.sh/docs/2.19/concepts/)
- [Envoy Circuit Breaking](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking)
