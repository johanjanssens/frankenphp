package frankenphp

import (
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/dunglas/frankenphp/internal/state"
	"github.com/shirou/gopsutil/v4/process"
)

const (
	// downscale idle threads every x seconds
	downScaleCheckTime = 5 * time.Second
	// max amount of threads stopped in one iteration of downScaleCheckTime
	maxTerminationCount = 10
)

var (
	cpuCount    = runtime.GOMAXPROCS(0)
	cpuProc     *process.Process
	cpuProcOnce sync.Once

	ErrMaxThreadsReached = errors.New("max amount of overall threads reached")

	scalingPolicy     ScalingPolicy = NewDefaultScalingPolicy()
	scaleChan         chan *frankenPHPContext
	autoScaledThreads = []*phpThread{}
	scalingMu         = new(sync.RWMutex)
)

// getCPUUsage returns the current process CPU usage as a fraction of total capacity (0.0-1.0).
// Returns 1.0 (max load) if the process handle is unavailable or an error occurs,
// which causes policies to block scale-up as a safe default.
func getCPUUsage() float64 {
	cpuProcOnce.Do(func() {
		cpuProc, _ = process.NewProcess(int32(os.Getpid()))
	})
	if cpuProc == nil {
		return 1.0
	}

	cpuPercent, err := cpuProc.Percent(0)
	if err != nil {
		return 1.0
	}

	return cpuPercent / (float64(cpuCount) * 100.0)
}

func initAutoScaling(mainThread *phpMainThread) {
	if mainThread.maxThreads <= mainThread.numThreads {
		scaleChan = nil
		return
	}

	scalingMu.Lock()
	scaleChan = make(chan *frankenPHPContext)
	maxScaledThreads := mainThread.maxThreads - mainThread.numThreads
	autoScaledThreads = make([]*phpThread, 0, maxScaledThreads)
	scalingMu.Unlock()

	go startUpscalingThreads(maxScaledThreads, scaleChan, mainThread.done)
	go startDownScalingThreads(mainThread.done)
}

func drainAutoScaling() {
	scalingMu.Lock()

	if globalLogger.Enabled(globalCtx, slog.LevelDebug) {
		globalLogger.LogAttrs(globalCtx, slog.LevelDebug, "shutting down autoscaling", slog.Int("autoScaledThreads", len(autoScaledThreads)))
	}

	scalingMu.Unlock()
}

func addRegularThread() (*phpThread, error) {
	thread := getInactivePHPThread()
	if thread == nil {
		return nil, ErrMaxThreadsReached
	}
	convertToRegularThread(thread)
	thread.state.WaitFor(state.Ready, state.ShuttingDown, state.Reserved)
	return thread, nil
}

func addWorkerThread(worker *worker) (*phpThread, error) {
	thread := getInactivePHPThread()
	if thread == nil {
		return nil, ErrMaxThreadsReached
	}
	convertToWorkerThread(thread, worker)
	thread.state.WaitFor(state.Ready, state.ShuttingDown, state.Reserved)
	return thread, nil
}

// scaleWorkerThread adds a worker PHP thread automatically
func scaleWorkerThread(worker *worker, rt RequestType) {
	scalingMu.Lock()
	defer scalingMu.Unlock()

	if !mainThread.state.Is(state.Ready) {
		return
	}

	thread, err := addWorkerThread(worker)
	if err != nil {
		if globalLogger.Enabled(globalCtx, slog.LevelWarn) {
			globalLogger.LogAttrs(globalCtx, slog.LevelWarn, "could not increase max_threads, consider raising this limit", slog.String("worker", worker.name), slog.Any("error", err))
		}

		return
	}

	thread.requestType = rt
	autoScaledThreads = append(autoScaledThreads, thread)

	if globalLogger.Enabled(globalCtx, slog.LevelInfo) {
		globalLogger.LogAttrs(globalCtx, slog.LevelInfo, "upscaling worker thread", slog.String("worker", worker.name), slog.String("type", string(rt)), slog.Int("thread", thread.threadIndex), slog.Int("num_threads", len(autoScaledThreads)))
	}
}

// scaleRegularThread adds a regular PHP thread automatically
func scaleRegularThread(rt RequestType) {
	scalingMu.Lock()
	defer scalingMu.Unlock()

	if !mainThread.state.Is(state.Ready) {
		return
	}

	thread, err := addRegularThread()
	if err != nil {
		if globalLogger.Enabled(globalCtx, slog.LevelWarn) {
			globalLogger.LogAttrs(globalCtx, slog.LevelWarn, "could not increase max_threads, consider raising this limit", slog.Any("error", err))
		}

		return
	}

	thread.requestType = rt
	autoScaledThreads = append(autoScaledThreads, thread)

	if globalLogger.Enabled(globalCtx, slog.LevelInfo) {
		globalLogger.LogAttrs(globalCtx, slog.LevelInfo, "upscaling regular thread", slog.String("type", string(rt)), slog.Int("thread", thread.threadIndex), slog.Int("num_threads", len(autoScaledThreads)))
	}
}

func startUpscalingThreads(maxScaledThreads int, scale chan *frankenPHPContext, done chan struct{}) {
	for {
		scalingMu.Lock()
		scaledThreadCount := len(autoScaledThreads)
		scalingMu.Unlock()
		if scaledThreadCount >= maxScaledThreads {
			// we have reached max_threads, check again later
			select {
			case <-done:
				return
			case <-time.After(downScaleCheckTime):
				continue
			}
		}

		select {
		case fc := <-scale:
			metrics := ScalingMetrics{
				CPUUsage:      getCPUUsage(),
				StallDuration: time.Since(fc.startedAt),
				ActiveThreads: scaledThreadCount,
				MaxThreads:    maxScaledThreads,
				RequestType:   fc.requestType,
			}

			if !scalingPolicy.ShouldScaleUp(metrics) {
				continue
			}

			// if the request has been stalled long enough, scale
			if fc.worker == nil {
				scaleRegularThread(fc.requestType)
				continue
			}

			// check for max worker threads here again in case requests overflowed while waiting
			if fc.worker.isAtThreadLimit() {
				if globalLogger.Enabled(globalCtx, slog.LevelInfo) {
					globalLogger.LogAttrs(globalCtx, slog.LevelInfo, "cannot scale worker thread, max threads reached for worker", slog.String("worker", fc.worker.name))
				}

				continue
			}

			scaleWorkerThread(fc.worker, fc.requestType)
		case <-done:
			return
		}
	}
}

func startDownScalingThreads(done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(downScaleCheckTime):
			deactivateThreads()
		}
	}
}

// deactivateThreads checks all threads and removes those that have been inactive for too long
func deactivateThreads() {
	// probe CPU before acquiring the lock — keeps Percent(0) window tight
	// so scale-up always has a fresh baseline, and gives policies CPU data
	cpuUsage := getCPUUsage()
	stoppedThreadCount := 0
	scalingMu.Lock()
	defer scalingMu.Unlock()
	for i := len(autoScaledThreads) - 1; i >= 0; i-- {
		thread := autoScaledThreads[i]

		// the thread might have been stopped otherwise, remove it
		if thread.state.Is(state.Reserved) {
			autoScaledThreads = append(autoScaledThreads[:i], autoScaledThreads[i+1:]...)
			continue
		}

		waitTime := thread.state.WaitTime()
		if stoppedThreadCount > maxTerminationCount || waitTime == 0 {
			continue
		}

		metrics := ScalingMetrics{
			CPUUsage:      cpuUsage,
			IdleDuration:  time.Duration(waitTime) * time.Millisecond,
			ActiveThreads: len(autoScaledThreads),
			RequestType:   thread.requestType,
		}

		// convert threads to inactive if the policy says so
		if thread.state.Is(state.Ready) && scalingPolicy.ShouldScaleDown(metrics) {
			convertToInactiveThread(thread)
			thread.requestType = RequestTypeDefault
			stoppedThreadCount++
			autoScaledThreads = append(autoScaledThreads[:i], autoScaledThreads[i+1:]...)

			if globalLogger.Enabled(globalCtx, slog.LevelInfo) {
				globalLogger.LogAttrs(globalCtx, slog.LevelInfo, "downscaling thread", slog.Int("thread", thread.threadIndex), slog.Int64("wait_time", waitTime), slog.Int("num_threads", len(autoScaledThreads)))
			}

			continue
		}

		// TODO: Completely stopping threads is more memory efficient
		// Some PECL extensions like #1296 will prevent threads from fully stopping (they leak memory)
		// Reactivate this if there is a better solution or workaround
		// if thread.state.Is(state.Inactive) && waitTime > maxThreadIdleTime.Milliseconds() {
		// 	logger.LogAttrs(nil, slog.LevelDebug, "auto-stopping thread", slog.Int("thread", thread.threadIndex))
		// 	thread.shutdown()
		// 	stoppedThreadCount++
		// 	autoScaledThreads = append(autoScaledThreads[:i], autoScaledThreads[i+1:]...)
		// 	continue
		// }
	}
}
