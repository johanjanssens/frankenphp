package frankenphp

import (
	"log/slog"
	"path/filepath"
	"time"
)

// Option instances allow to configure FrankenPHP.
type Option func(h *opt) error

// opt contains the available options.
//
// If you change this, also update the Caddy module and the documentation.
type opt struct {
	numThreads   int
	maxThreads   int
	workers      []workerOpt
	logger       *slog.Logger
	metrics      Metrics
	phpIni       map[string]string
	maxWaitTime  time.Duration
	documentRoot string
}

type workerOpt struct {
	name         string
	fileName     string
	num          int
	env          PreparedEnv
	watch        []string
	documentRoot string
}

// WithNumThreads configures the number of PHP threads to start.
func WithNumThreads(numThreads int) Option {
	return func(o *opt) error {
		o.numThreads = numThreads

		return nil
	}
}

func WithMaxThreads(maxThreads int) Option {
	return func(o *opt) error {
		o.maxThreads = maxThreads

		return nil
	}
}

func WithMetrics(m Metrics) Option {
	return func(o *opt) error {
		o.metrics = m

		return nil
	}
}

// WithWorkers configures the PHP workers to start
func WithWorkers(name string, fileName string, num int, env map[string]string, watch []string) Option {
	return func(o *opt) error {
		o.workers = append(o.workers, workerOpt{name, fileName, num, PrepareEnv(env), watch, ""})
		return nil
	}
}

// WithLogger configures the global logger to use.
func WithLogger(l *slog.Logger) Option {
	return func(o *opt) error {
		o.logger = l

		return nil
	}
}

// WithPhpIni configures user defined PHP ini settings.
func WithPhpIni(overrides map[string]string) Option {
	return func(o *opt) error {
		o.phpIni = overrides
		return nil
	}
}

// WithMaxWaitTime configures the max time a request may be stalled waiting for a thread.
func WithMaxWaitTime(maxWaitTime time.Duration) Option {
	return func(o *opt) error {
		o.maxWaitTime = maxWaitTime

		return nil
	}
}

// WithDocumentRoot sets the root directory of the PHP application.
// if resolveSymlink is true, oath declared as root directory will be resolved
// to its absolute value after the evaluation of any symbolic links.
// Due to the nature of PHP opcache, root directory path is cached: when
// using a symlinked directory as root this could generate errors when
// symlink is changed without PHP being restarted; enabling this
// directive will set $_SERVER['DOCUMENT_ROOT'] to the real directory path.
func WithDocumentRoot(documentRoot string, resolveSymlink bool) Option {
	return func(o *opt) (err error) {
		v, ok := documentRootCache.Load(documentRoot)
		if !ok {
			// make sure file root is absolute
			v, err = safeAbsPath(documentRoot)
			if err != nil {
				return err
			}

			// prevent the cache to grow forever, this is a totally arbitrary value
			if documentRootCacheLen.Load() < 1024 {
				documentRootCache.LoadOrStore(documentRoot, v)
				documentRootCacheLen.Add(1)
			}
		}

		if resolveSymlink {
			if v, err = filepath.EvalSymlinks(v.(string)); err != nil {
				return err
			}
		}

		o.documentRoot = v.(string)

		return nil
	}
}

// WithResolvedDocumentRoot is similar to WithDocumentRoot
// but doesn't do any checks or resolving on the path to improve performance.
func WithResolvedDocumentRoot(documentRoot string) Option {
	return func(o *opt) error {
		o.documentRoot = documentRoot
		return nil
	}
}
