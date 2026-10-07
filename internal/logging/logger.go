// Package logging constructs the zap logger used throughout the Gateway.
package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

func New(cfg config.LoggingConfig) (*zap.Logger, error) {
	var level zapcore.Level
	if err := level.Set(cfg.Level); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", cfg.Level, err)
	}

	var zapCfg zap.Config
	switch cfg.Format {
	case "json":
		zapCfg = zap.NewProductionConfig()
	case "console", "":
		zapCfg = zap.NewDevelopmentConfig()
	default:
		return nil, fmt.Errorf("invalid log format %q (want console or json)", cfg.Format)
	}
	zapCfg.Level = zap.NewAtomicLevelAt(level)

	logger, err := zapCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("building logger: %w", err)
	}
	return logger, nil
}
