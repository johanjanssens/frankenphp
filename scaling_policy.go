package frankenphp

import (
	"time"
)

// RequestType identifies the kind of request for scaling decisions, metrics, and logging.
type RequestType string

const (
	// RequestTypeDefault is the zero value — standard untagged requests.
	RequestTypeDefault RequestType = ""
	// RequestTypeAsync identifies internal subrequests and async task execution.
	RequestTypeAsync RequestType = "async"
)

// ScalingPolicy controls when threads are added or removed.
// Implement this interface to customize autoscaling behavior.
type ScalingPolicy interface {
	// ShouldScaleUp is called when a request has been stalled.
	// Returns true if a new thread should be added.
	ShouldScaleUp(metrics ScalingMetrics) bool

	// ShouldScaleDown is called periodically for each idle thread.
	// Returns true if the thread should be deactivated.
	ShouldScaleDown(metrics ScalingMetrics) bool
}

// ScalingMetrics provides context for scaling decisions.
type ScalingMetrics struct {
	// CPUUsage is the current process CPU usage as a fraction of total capacity (0.0-1.0).
	// For example, 0.5 means 50% of all available CPU cores are in use.
	CPUUsage float64

	// StallDuration is how long the triggering request has been waiting for a thread.
	// Only meaningful in ShouldScaleUp context.
	StallDuration time.Duration

	// IdleDuration is how long the thread has been idle.
	// Only meaningful in ShouldScaleDown context.
	IdleDuration time.Duration

	// ActiveThreads is the number of currently active autoscaled threads.
	ActiveThreads int

	// MaxThreads is the maximum number of threads that can be autoscaled.
	MaxThreads int

	// RequestType identifies the kind of request that triggered scaling.
	// Policies can use this to apply different strategies per request type.
	// See RequestTypeDefault, RequestTypeAsync.
	RequestType RequestType
}

// DefaultScalingPolicy matches FrankenPHP's built-in scaling behavior:
// conservative scale-up with a minimum stall time and CPU gate,
// scale-down after idle timeout.
type DefaultScalingPolicy struct {
	// MinStallTime is the minimum time a request must be stalled before triggering scale-up.
	// Default: 5ms
	MinStallTime time.Duration

	// MaxCPUUsage is the CPU usage threshold (0.0-1.0) above which scale-up is blocked.
	// Default: 0.8
	MaxCPUUsage float64

	// MaxIdleTime is how long a thread may be idle before being deactivated.
	// Default: 5s
	MaxIdleTime time.Duration
}

// NewDefaultScalingPolicy returns a DefaultScalingPolicy with FrankenPHP's built-in defaults.
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

// ImmediateScalingPolicy scales up instantly when demand exists,
// only gated by CPU usage. Designed for internal parallelism
// workloads (e.g. async subrequests) where latency matters more than
// resource conservation.
type ImmediateScalingPolicy struct {
	// MaxCPUUsage is the CPU usage threshold (0.0-1.0) above which scale-up is blocked.
	// Default: 0.95
	MaxCPUUsage float64

	// MaxIdleTime is how long a thread may be idle before being deactivated.
	// Default: 5s
	MaxIdleTime time.Duration
}

// NewImmediateScalingPolicy returns an ImmediateScalingPolicy with sensible defaults.
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
