package logp

import (
	"flag"
	"io/ioutil"
	golog "log"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

var (
	_log unsafe.Pointer // Pointer to a coreLogger. Access via atomic.LoadPointer.
)

func init() {
	storeLogger(&coreLogger{
		selectors:    map[string]struct{}{},
		rootLogger:   zap.NewNop(),
		globalLogger: zap.NewNop(),
	})
}

type coreLogger struct {
	selectors    map[string]struct{}
	rootLogger   *zap.Logger
	globalLogger *zap.Logger
	observedLogs *observer.ObservedLogs
}

// Configure configures the logp package.
func Configure(cfg Config) error {
	var (
		sink         zapcore.Core
		observedLogs *observer.ObservedLogs
		err          error
	)

	// Build a single output (stderr has priority if more than one are enabled).
	switch {
	case cfg.toObserver:
		sink, observedLogs = observer.New(cfg.Level.zapLevel())
	case cfg.toIODiscard:
		sink, err = makeDiscardOutput(cfg)
	case cfg.ToStderr:
		sink, err = makeStderrOutput(cfg)
	case cfg.ToSyslog:
		sink, err = makeSyslogOutput(cfg)
	case cfg.ToFiles:
		fallthrough
	default:
		sink, err = makeFileOutput(cfg)
	}
	if err != nil {
		return errors.Wrap(err, "failed to build log output")
	}

	// Enabled selectors when debug is enabled.
	selectors := make(map[string]struct{}, len(cfg.Selectors))
	if cfg.Level.Enabled(DebugLevel) && len(cfg.Selectors) > 0 {
		for _, sel := range cfg.Selectors {
			selectors[sel] = struct{}{}
		}

		// Default to all enabled if no selectors are specified.
		if len(selectors) == 0 {
			selectors["*"] = struct{}{}
		}

		if _, enabled := selectors["stdlog"]; !enabled {
			// Disable standard logging by default (this is sometimes used by
			// libraries and we don't want their spam).
			golog.SetOutput(ioutil.Discard)
		}

		sink = selectiveWrapper(sink, selectors)
	}

	root := zap.New(sink, makeOptions(cfg)...)
	storeLogger(&coreLogger{
		selectors:    selectors,
		rootLogger:   root,
		globalLogger: root.WithOptions(zap.AddCallerSkip(1)),
		observedLogs: observedLogs,
	})
	return nil
}

// DevelopmentSetup configures the logger in development mode at debug level.
// By default the output goes to stderr.
func DevelopmentSetup(options ...Option) error {
	cfg := Config{
		Level:       DebugLevel,
		ToStderr:    true,
		development: true,
		addCaller:   true,
	}
	for _, apply := range options {
		apply(&cfg)
	}
	return Configure(cfg)
}

// TestingSetup configures logging by calling DevelopmentSetup if and only if
// verbose testing is enabled (as in 'go test -v').
func TestingSetup(options ...Option) error {
	// Use the flag to avoid a dependency on the testing package.
	f := flag.Lookup("test.v")
	if f != nil && f.Value.String() == "true" {
		return DevelopmentSetup(options...)
	}
	return nil
}

// ObserverLogs provides the list of logs generated during the observation
// process.
func ObserverLogs() *observer.ObservedLogs {
	return loadLogger().observedLogs
}

// Sync flushes any buffered log entries. Applications should take care to call
// Sync before exiting.
func Sync() error {
	return loadLogger().rootLogger.Sync()
}

func makeOptions(cfg Config) []zap.Option {
	var options []zap.Option
	if cfg.addCaller {
		options = append(options, zap.AddCaller())
	}
	if cfg.development {
		options = append(options, zap.Development())
	}
	return options
}

func makeStderrOutput(cfg Config) (zapcore.Core, error) {
	stderr := zapcore.Lock(os.Stderr)
	return zapcore.NewCore(buildEncoder(cfg), stderr, cfg.Level.zapLevel()), nil
}

func makeDiscardOutput(cfg Config) (zapcore.Core, error) {
	discard := zapcore.AddSync(ioutil.Discard)
	return zapcore.NewCore(buildEncoder(cfg), discard, cfg.Level.zapLevel()), nil
}

func makeSyslogOutput(cfg Config) (zapcore.Core, error) {
	return newSyslog(buildEncoder(cfg), cfg.Level.zapLevel())
}

func makeFileOutput(cfg Config) (zapcore.Core, error) {
	name := cfg.Beat
	if cfg.Files.Name != "" {
		name = cfg.Files.Name
	}

	exPath, err := os.Executable()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get heplify dir")
	}
	exPathDir := filepath.Dir(exPath)
	filename := exPathDir + "/" + name
	//filename := paths.Resolve(paths.Logs, filepath.Join(cfg.Files.Path, name))

	rotator, err := NewFileRotator(filename,
		MaxSizeBytes(cfg.Files.MaxSize),
		MaxBackups(cfg.Files.MaxBackups),
		Permissions(os.FileMode(cfg.Files.Permissions)),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create file rotator")
	}

	return zapcore.NewCore(buildEncoder(cfg), rotator, cfg.Level.zapLevel()), nil
}

func globalLogger() *zap.Logger {
	return loadLogger().globalLogger
}

func loadLogger() *coreLogger {
	p := atomic.LoadPointer(&_log)
	return (*coreLogger)(p)
}

func storeLogger(l *coreLogger) {
	if old := loadLogger(); old != nil {
		old.rootLogger.Sync()
	}
	atomic.StorePointer(&_log, unsafe.Pointer(l))
}
