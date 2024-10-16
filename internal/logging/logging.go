package logging

import (
	"evm-cache/internal/config"
	"fmt"
	"os"
	"time"

	"path/filepath"

	"github.com/natefinch/lumberjack"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Logger = zap.SugaredLogger

var (
	globalLogger *zap.SugaredLogger
)

func Setup(cfg *config.Config) error {

	baseLogDir := cfg.Logging.LogDirectory
	if baseLogDir == "" {
		baseLogDir = "assets/logs"
	}

	if err := os.MkdirAll(baseLogDir, 0755); err != nil {
		return fmt.Errorf("error creating log directory: %w", err)
	}

	logFileName := cfg.Logging.LogFileName
	if logFileName == "" {
		logFileName = "app.log"
	}

	// Construct the full path for the log file
	logFilePath := filepath.Join(baseLogDir, logFileName)

	// Create a custom encoder config
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(time.RFC3339)

	jsonEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// Parse the desired log level
	level := zap.NewAtomicLevelAt(zapcore.InfoLevel)
	if cfg.Logging.Level != "" {
		if err := level.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
			return fmt.Errorf("error parsing log level: %w", err)
		}
	}

	// Create a custom core that writes JSON to both rotated file and console
	core := zapcore.NewTee(
		zapcore.NewCore(jsonEncoder, getRotatedFileWriter(logFilePath, cfg.Logging.MaxLogFileSize, cfg.Logging.MaxBackups, cfg.Logging.MaxAge), level),
		zapcore.NewCore(jsonEncoder, zapcore.AddSync(os.Stdout), level),
	)

	logger := zap.New(core)

	globalLogger = logger.Sugar()
	return nil
}

func getRotatedFileWriter(filename string, maxSize, maxBackups, maxAge int) zapcore.WriteSyncer {
	return zapcore.AddSync(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     maxAge,
		Compress:   true,
		LocalTime:  true,
	})
}

func GetLogger() *zap.SugaredLogger {
	return globalLogger
}
