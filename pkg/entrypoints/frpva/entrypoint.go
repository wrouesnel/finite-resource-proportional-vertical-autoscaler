package frpva

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"

	"github.com/wrouesnel/ctxstdio"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/version"
	"github.com/wrouesnel/kongutil"

	"github.com/chigopher/pathlib"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/alecthomas/kong"
)

// Name is the name of this subapplication.
const Name = "frpva"

// CLIConfig is the root command line configuration parsed by kong.
type CLIConfig struct {
	Version kong.VersionFlag `help:"Show version number"`

	CpuProfile bool `help:"Enable CPU Profiling"`
	MemProfile bool `help:"Enable Memory Profiling"`

	CpuProfileOutputPath string `default:"${application_name}.cpu.pprof"      help:"CPU Profiling Output Path"`
	MemProfileOutputPath string `default:"${application_name}.mem.pprof"      help:"Memory Profiling Output Path"`
	GoRoutineOutputPath  string `default:"${application_name}.goroutines.txt" help:"Go Routine Dump Output Path"`

	Logging struct {
		Level       string   `default:"info"    help:"logging level"`
		Format      string   `default:"console" enum:"console,json"    help:"logging format (${enum})"`
		OutputPaths []string `default:"stderr"  help:"Paths to log to"`
	} `embed:"" prefix:"log-"`

	ConfigFile *pathlib.Path `default:"${default_configpath}" help:"Path to top-level configuration file"`

	Run RunCmd `cmd:"" default:"1" help:"Run the application (default)"`
}

// kongExit carries an exit code requested by kong (e.g. for --help or --version) out of the
// parser so Entrypoint can return it instead of the process exiting.
type kongExit struct {
	code int
}

// nextFreePath returns the first of base.0, base.1, ... which does not exist yet, so repeated
// profile dumps don't overwrite each other.
func nextFreePath(base string) (string, bool) {
	for idx := 0; idx <= 1000000; idx++ {
		outputPath := fmt.Sprintf("%v.%v", base, idx)
		if _, err := os.Stat(outputPath); errors.Is(err, os.ErrNotExist) {
			return outputPath, true
		}
	}
	return "", false
}

// Entrypoint is the real application entrypoint. This structure allows test packages to E2E-style tests invoking commmands
// as though they are on the command line, but using built-in coverage tools. Stub-main under the `cmd` package calls this
// function with os.Args[1:].
func Entrypoint(ctx context.Context, args []string) (exitCode int) {
	stdOut := ctxstdio.StdOut(ctx)
	stdErr := ctxstdio.StdErr(ctx)

	defer func() {
		if r := recover(); r != nil {
			exit, ok := r.(kongExit)
			if !ok {
				panic(r)
			}
			exitCode = exit.code
		}
	}()

	appCtx, appCancel := context.WithCancel(ctx)
	defer appCancel()

	deferredLogs := []string{}

	// Command line parsing can now happen
	vars := kong.Vars{
		"version":            version.Version,
		"default_configpath": fmt.Sprintf("%s.yml", version.Name),
		"application_name":   fmt.Sprintf("%s-%s", version.Name, Name),
	}

	// cli is the root entrypoint config parsed by kong and made available to subcommands via
	// a binding.
	var cli CLIConfig

	parser, err := kong.New(&cli,
		kong.Name(Name),
		kong.Description(version.Description),
		kong.DefaultEnvars(strings.ReplaceAll(version.Name, "-", "_")),
		kong.Writers(stdOut, stdErr),
		kong.Exit(func(code int) { panic(kongExit{code: code}) }),
		kongutil.PathlibMapper(),
		vars)
	if err != nil {
		_, _ = fmt.Fprintf(stdErr, "Failure while building command line parser: %v\n", err)
		return 1
	}

	kongCtx, err := parser.Parse(args)
	parser.FatalIfErrorf(err)

	// Initialize logging as soon as possible
	logConfig := zap.NewProductionConfig()
	if err := logConfig.Level.UnmarshalText([]byte(cli.Logging.Level)); err != nil {
		deferredLogs = append(deferredLogs, err.Error())
	}
	logConfig.Encoding = cli.Logging.Format
	logConfig.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	if cli.Logging.Format == "console" {
		logConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	logConfig.OutputPaths = cli.Logging.OutputPaths

	logger, err := logConfig.Build()
	if err != nil {
		// Error unhandled since this is a very early failure
		_, _ = io.WriteString(stdErr, "Failure while building logger\n")
		return 1
	}
	defer func() { _ = logger.Sync() }()

	collectMemoryProfile := func() {
		if cli.MemProfileOutputPath == "" {
			logger.Warn("Memory profile requested but writing output disabled")
			return
		}
		outputPath, ok := nextFreePath(cli.MemProfileOutputPath)
		if !ok {
			logger.Error("Didn't find a usable file to write the dump to!")
			return
		}
		logger.Info("Writing Memory Profile: Started", zap.String("filename", outputPath))
		f, err := os.Create(outputPath)
		if err != nil {
			logger.Error("Could not create memory profile output", zap.String("filename", outputPath))
			return
		}
		defer f.Close()
		runtime.GC()
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			logger.Error("Writing Memory Profile: Error", zap.Error(err))
			return
		}
		logger.Info("Writing Memory Profile: Success", zap.String("filename", outputPath))
	}

	logGoRoutineProfile := func() {
		if cli.GoRoutineOutputPath == "" {
			logger.Warn("Go routine profile requested but writing output disabled")
			return
		}
		outputPath, ok := nextFreePath(cli.GoRoutineOutputPath)
		if !ok {
			logger.Error("Didn't find a usable file to write the dump to!")
			return
		}
		logger.Info("Writing Go Routine Stack Dump: Start", zap.String("filename", outputPath))
		f, err := os.Create(outputPath)
		if err != nil {
			logger.Error("Could not create goroutines output", zap.String("filename", outputPath))
			return
		}
		defer f.Close()

		if err := pprof.Lookup("goroutine").WriteTo(f, 1); err != nil {
			logger.Error("Writing Go Routine Stack Dump: Error", zap.Error(err))
		} else {
			logger.Info("Writing Go Routine Stack Dump: Success", zap.String("filename", outputPath))
		}
	}
	// Install as the global logger
	defer zap.ReplaceGlobals(logger)()

	logger.Debug("Configuring signal handling")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, handledSignals...)
	// Stop signal delivery on return so repeated calls (e.g. from tests) don't leak handlers.
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
	}()
	sigCtx, cancelFn := context.WithCancel(appCtx)
	defer cancelFn()
	go func() {
		for sig := range sigCh {
			switch sig {
			case memProfileSignal:
				logger.Info("Caught signal - collecting memory profile", zap.String("signal", sig.String()))
				collectMemoryProfile()
			case goRoutineDumpSignal:
				logger.Info("Caught signal - dumping current goroutines", zap.String("signal", sig.String()))
				logGoRoutineProfile()
			default:
				logger.Info("Caught signal - exiting", zap.String("signal", sig.String()))
				cancelFn()
			}
		}
		logger.Debug("Signal handling channel closed")
	}()

	// Emit deferred logs
	for _, line := range deferredLogs {
		logger.Error(line)
	}

	// config is the main configuration file.
	config := EntrypointConfig{}
	if err := UnmarshalConfig(cli.ConfigFile, &config); err != nil {
		logger.Error("Could not load configuration file", zap.String("filename", cli.ConfigFile.String()), zap.Error(err))
		return 1
	}

	// Main application context
	kongCtx.BindTo(sigCtx, (*context.Context)(nil))
	// Main command line
	kongCtx.Bind(&cli)
	// Main config file
	kongCtx.Bind(&config)
	// kongCtx if needed.
	kongCtx.Bind(kongCtx)

	if cli.CpuProfile {
		logger.Info("Writing CPU profile", zap.String("filename", cli.CpuProfileOutputPath))
		f, err := os.Create(cli.CpuProfileOutputPath)
		if err != nil {
			logger.Error("Could not create CPU profile output", zap.String("filename", cli.CpuProfileOutputPath), zap.Error(err))
			return 1
		}
		defer f.Close() // error handling omitted for example
		if err := pprof.StartCPUProfile(f); err != nil {
			logger.Error("Could not start CPU profile", zap.Error(err))
			return 1
		}
		defer pprof.StopCPUProfile()
	}

	runErr := kongCtx.Run()
	if runErr != nil {
		logger.Error("Error from command", zap.Error(runErr))
	}

	if cli.MemProfile {
		collectMemoryProfile()
	}

	if runErr != nil {
		logger.Warn("Exiting with error")
		return 1
	}
	logger.Info("Exiting normally")
	return 0
}
