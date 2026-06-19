package logger

import (
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LoggingConfig defines the structure for log settings.
type LoggingConfig struct {
	Level       string `yaml:"level"`
	Format      string `yaml:"format"`
	OutputPath  string `yaml:"output_path"`
	Development bool   `yaml:"development"`
}

var (
	globalLogger *zap.Logger
	atomicLevel  zap.AtomicLevel
)

// init ensures there's a safe default logger available before configuration is loaded.
func init() {
	atomicLevel = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	config := zap.NewProductionEncoderConfig()
	config.TimeKey = "timestamp"
	config.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncodeLevel = zapcore.CapitalColorLevelEncoder

	encoder := zapcore.NewConsoleEncoder(config)
	core := zapcore.NewCore(encoder, zapcore.AddSync(os.Stdout), atomicLevel)
	globalLogger = zap.New(core)
}

// InitLogger re-initializes the global logger based on the provided configuration.
func InitLogger(cfg *LoggingConfig) error {
	// 1. Fallback Defaults
	if cfg == nil {
		cfg = &LoggingConfig{}
	}
	if cfg.Level == "" {
		cfg.Level = "INFO"
	}
	if cfg.Format == "" {
		cfg.Format = "console"
	}
	if cfg.OutputPath == "" {
		cfg.OutputPath = "stdout"
	}

	// 2. Parse Level and Set AtomicLevel
	SetLevel(cfg.Level)

	// 3. Encoder Configuration
	var encoder zapcore.Encoder
	var encoderConfig zapcore.EncoderConfig

	if cfg.Development {
		encoderConfig = zap.NewDevelopmentEncoderConfig()
	} else {
		encoderConfig = zap.NewProductionEncoderConfig()
		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}

	if strings.ToLower(cfg.Format) == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		// Console format
		if cfg.Development {
			encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		} else {
			encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
		}
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	// 4. Output Path
	var sink zapcore.WriteSyncer
	if strings.ToLower(cfg.OutputPath) == "stdout" {
		sink = zapcore.AddSync(os.Stdout)
	} else if strings.ToLower(cfg.OutputPath) == "stderr" {
		sink = zapcore.AddSync(os.Stderr)
	} else {
		// Local file path
		file, err := os.OpenFile(cfg.OutputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		sink = zapcore.AddSync(file)
	}

	// 5. Build Core and swap global logger safely
	core := zapcore.NewCore(encoder, sink, atomicLevel)

	var opts []zap.Option
	if cfg.Development {
		opts = append(opts, zap.Development())
	}

	globalLogger = zap.New(core, opts...)
	return nil
}

// SetLevel dynamically changes the logging level at runtime.
func SetLevel(levelStr string) {
	level, err := zapcore.ParseLevel(levelStr)
	if err != nil {
		level = zapcore.InfoLevel
	}
	atomicLevel.SetLevel(level)
}

// Info logs a message at InfoLevel with structured, zero-allocation fields.
func Info(msg string, fields ...zap.Field) {
	globalLogger.Info(msg, fields...)
}

// Debug logs a message at DebugLevel with structured, zero-allocation fields.
func Debug(msg string, fields ...zap.Field) {
	globalLogger.Debug(msg, fields...)
}

// Warn logs a message at WarnLevel with structured, zero-allocation fields.
func Warn(msg string, fields ...zap.Field) {
	globalLogger.Warn(msg, fields...)
}

// Error logs a message at ErrorLevel with structured, zero-allocation fields.
func Error(msg string, fields ...zap.Field) {
	globalLogger.Error(msg, fields...)
}

// Fatal logs a message at FatalLevel with structured, zero-allocation fields.
func Fatal(msg string, fields ...zap.Field) {
	globalLogger.Fatal(msg, fields...)
}

// Sync flushes any buffered log entries.
func Sync() error {
	return globalLogger.Sync()
}

// With returns a new zap.Logger with the given fields attached.
func With(fields ...zap.Field) *zap.Logger {
	return globalLogger.With(fields...)
}
