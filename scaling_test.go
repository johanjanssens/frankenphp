package frankenphp

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dunglas/frankenphp/internal/state"
	"github.com/stretchr/testify/assert"
)

func TestScaleARegularThreadUpAndDown(t *testing.T) {
	t.Cleanup(Shutdown)

	assert.NoError(t, Init(
		WithNumThreads(1),
		WithMaxThreads(2),
	))

	autoScaledThread := phpThreads[1]

	// scale up
	scaleRegularThread(RequestTypeDefault)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())
	assert.IsType(t, &regularThread{}, autoScaledThread.handler)

	// on down-scale, the thread will be marked as inactive
	setLongWaitTime(t, autoScaledThread)
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler)
}

func TestScaleAWorkerThreadUpAndDown(t *testing.T) {
	t.Cleanup(Shutdown)

	workerName := "worker1"
	workerPath := filepath.Join(testDataPath, "transition-worker-1.php")
	assert.NoError(t, Init(
		WithNumThreads(2),
		WithMaxThreads(3),
		WithWorkers(workerName, workerPath, 1,
			WithWorkerEnv(map[string]string{}),
			WithWorkerWatchMode([]string{}),
			WithWorkerMaxFailures(0),
		),
	))

	autoScaledThread := phpThreads[2]

	// scale up
	scaleWorkerThread(workersByPath[workerPath], RequestTypeDefault)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())

	// on down-scale, the thread will be marked as inactive
	setLongWaitTime(t, autoScaledThread)
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler)
}

func TestMaxIdleTimePreventsEarlyDeactivation(t *testing.T) {
	t.Cleanup(Shutdown)

	assert.NoError(t, Init(
		WithNumThreads(1),
		WithMaxThreads(2),
		WithMaxIdleTime(time.Hour),
	))

	autoScaledThread := phpThreads[1]

	// scale up
	scaleRegularThread(RequestTypeDefault)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())

	// set wait time to 30 minutes (less than 1 hour max idle time)
	autoScaledThread.state.SetWaitTime(time.Now().Add(-30 * time.Minute))
	deactivateThreads()
	assert.IsType(t, &regularThread{}, autoScaledThread.handler, "thread should still be active after 30min with 1h max idle time")

	// set wait time to over 1 hour (exceeds max idle time)
	autoScaledThread.state.SetWaitTime(time.Now().Add(-time.Hour - time.Minute))
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler, "thread should be deactivated after exceeding max idle time")
}

func TestImmediateScalingPolicyScalesWithoutDelay(t *testing.T) {
	t.Cleanup(Shutdown)

	assert.NoError(t, Init(
		WithNumThreads(1),
		WithMaxThreads(2),
		WithScalingPolicy(NewImmediateScalingPolicy()),
	))

	autoScaledThread := phpThreads[1]

	// ImmediateScalingPolicy should allow immediate scale-up
	scaleRegularThread(RequestTypeDefault)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())
	assert.IsType(t, &regularThread{}, autoScaledThread.handler)

	// down-scale still works with default idle time
	setLongWaitTime(t, autoScaledThread)
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler)
}

func TestScalingPolicyOverridesMaxIdleTime(t *testing.T) {
	t.Cleanup(Shutdown)

	// WithMaxIdleTime is set to 1 hour, but the explicit policy uses 1 second
	policy := &DefaultScalingPolicy{
		MinStallTime: 5 * time.Millisecond,
		MaxCPUUsage:  0.8,
		MaxIdleTime:  time.Second,
	}

	assert.NoError(t, Init(
		WithNumThreads(1),
		WithMaxThreads(2),
		WithMaxIdleTime(time.Hour),
		WithScalingPolicy(policy),
	))

	autoScaledThread := phpThreads[1]

	// scale up
	scaleRegularThread(RequestTypeDefault)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())

	// set wait time to 2 seconds — exceeds the policy's 1s, not the option's 1h
	autoScaledThread.state.SetWaitTime(time.Now().Add(-2 * time.Second))
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler, "explicit policy should override WithMaxIdleTime")
}

func TestRequestTypeInheritedByThread(t *testing.T) {
	t.Cleanup(Shutdown)

	assert.NoError(t, Init(
		WithNumThreads(1),
		WithMaxThreads(2),
	))

	autoScaledThread := phpThreads[1]

	// scale up with a request type
	scaleRegularThread(RequestTypeAsync)
	assert.Equal(t, state.Ready, autoScaledThread.state.Get())
	assert.Equal(t, RequestTypeAsync, autoScaledThread.requestType)

	// after deactivation, request type is cleared
	setLongWaitTime(t, autoScaledThread)
	deactivateThreads()
	assert.IsType(t, &inactiveThread{}, autoScaledThread.handler)
	assert.Equal(t, RequestTypeDefault, autoScaledThread.requestType)
}

func TestDefaultScalingPolicyShouldScaleUp(t *testing.T) {
	p := NewDefaultScalingPolicy()

	// Should scale: stall >= 5ms, CPU below threshold
	assert.True(t, p.ShouldScaleUp(ScalingMetrics{
		CPUUsage:      0.5,
		StallDuration: 10 * time.Millisecond,
	}))

	// Should not scale: stall too short
	assert.False(t, p.ShouldScaleUp(ScalingMetrics{
		CPUUsage:      0.5,
		StallDuration: 1 * time.Millisecond,
	}))

	// Should not scale: CPU too high
	assert.False(t, p.ShouldScaleUp(ScalingMetrics{
		CPUUsage:      0.9,
		StallDuration: 10 * time.Millisecond,
	}))
}

func TestImmediateScalingPolicyShouldScaleUp(t *testing.T) {
	p := NewImmediateScalingPolicy()

	// Should scale immediately even with zero stall time
	assert.True(t, p.ShouldScaleUp(ScalingMetrics{
		CPUUsage:      0.5,
		StallDuration: 0,
	}))

	// Should not scale: CPU too high
	assert.False(t, p.ShouldScaleUp(ScalingMetrics{
		CPUUsage:      0.96,
		StallDuration: 0,
	}))
}

func TestDefaultScalingPolicyShouldScaleDown(t *testing.T) {
	p := NewDefaultScalingPolicy()

	// Should not scale down: idle time within threshold
	assert.False(t, p.ShouldScaleDown(ScalingMetrics{
		IdleDuration: 3 * time.Second,
	}))

	// Should scale down: idle time exceeds threshold
	assert.True(t, p.ShouldScaleDown(ScalingMetrics{
		IdleDuration: 6 * time.Second,
	}))
}

func setLongWaitTime(t *testing.T, thread *phpThread) {
	t.Helper()

	thread.state.SetWaitTime(time.Now().Add(-time.Hour))
}
