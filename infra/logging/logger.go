package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	otelsdk "github.com/webitel/webitel-go-kit/infra/otel/sdk"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"

	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/log/otlp"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/log/stdout"
)

func New(cfg *config.Config, lifecycle fx.Lifecycle) (*slog.Logger, error) {
	settings := cfg.Log
	options := &slog.HandlerOptions{Level: parseLevel(settings.Level)}
	handlers := make([]slog.Handler, 0, 3)
	var logFile *os.File

	if settings.Console {
		handlers = append(handlers, newHandler(os.Stdout, settings.JSON, options))
	}

	if settings.File != "" {
		file, err := os.OpenFile(settings.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("logging: open file: %w", err)
		}
		logFile = file

		lifecycle.Append(fx.Hook{
			OnStop: func(context.Context) error {
				return file.Close()
			},
		})
		handlers = append(handlers, newHandler(file, settings.JSON, options))
	}

	if settings.Otel {
		otelHandler := otelslog.NewHandler("email-service")
		res := resource.NewSchemaless(
			semconv.ServiceName(model.ServiceName),
			semconv.ServiceVersion(model.Version),
			semconv.ServiceNamespace(model.ServiceNamespace),
		)

		shutdown, err := otelsdk.Configure(
			context.Background(),
			otelsdk.WithResource(res),
			otelsdk.WithLogBridge(func() {
				handlers = append(handlers, otelHandler)
			}),
		)
		if err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			return nil, fmt.Errorf("logging: configure OpenTelemetry: %w", err)
		}

		lifecycle.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				return shutdown(ctx)
			},
		})
	}

	if len(handlers) == 0 {
		handlers = append(handlers, newHandler(os.Stdout, settings.JSON, options))
	}

	log := slog.New(joinHandlers(handlers...))
	slog.SetDefault(log)

	return log, nil
}

func NewFxEventLogger(log *slog.Logger) fxevent.Logger {
	fxLog := &fxevent.SlogLogger{Logger: log}
	fxLog.UseLogLevel(slog.LevelDebug)

	return fxLog
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newHandler(writer io.Writer, json bool, options *slog.HandlerOptions) slog.Handler {
	if json {
		return slog.NewJSONHandler(writer, options)
	}

	return slog.NewTextHandler(writer, options)
}
