package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	ojsotel "github.com/openjobspec/ojs-go-backend-common/otel"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	ojsgrpc "github.com/openjobspec/ojs-backend-nats/internal/grpc"
	"github.com/openjobspec/ojs-backend-nats/internal/metrics"
	natsbackend "github.com/openjobspec/ojs-backend-nats/internal/nats"
	"github.com/openjobspec/ojs-backend-nats/internal/scheduler"
	"github.com/openjobspec/ojs-backend-nats/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := server.LoadConfig()
	if cfg.APIKey == "" && !cfg.AllowInsecureNoAuth {
		slog.Error("refusing to start without API authentication", "hint", "set OJS_API_KEY or OJS_ALLOW_INSECURE_NO_AUTH=true for local development")
		os.Exit(1)
	}
	if cfg.AllowInsecureNoAuth {
		slog.Warn("⚠️  RUNNING WITHOUT AUTHENTICATION — this is intended for local development only. Set OJS_API_KEY for any shared or production environment.")
	}

	// Initialize OpenTelemetry (opt-in via OJS_OTEL_ENABLED or OTEL_EXPORTER_OTLP_ENDPOINT)
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if ep := os.Getenv("OJS_OTEL_ENDPOINT"); ep != "" {
		otelEndpoint = ep
	}
	otelShutdown, err := ojsotel.Init(context.Background(), ojsotel.Config{
		ServiceName:    "ojs-backend-nats",
		ServiceVersion: core.OJSVersion,
		Enabled:        os.Getenv("OJS_OTEL_ENABLED") == "true" || otelEndpoint != "",
		Endpoint:       otelEndpoint,
	})
	if err != nil {
		slog.Error("failed to initialize OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	// Connect to NATS
	backend, err := natsbackend.New(cfg.NatsURL)
	if err != nil {
		slog.Error("failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer backend.Close()

	slog.Info("connected to NATS", "url", cfg.NatsURL)

	// Initialize Prometheus server info metric
	metrics.Init(core.OJSVersion, "nats")

	// Start background scheduler
	sched := scheduler.New(backend)
	sched.Start()
	defer sched.Stop()

	// Initialize real-time Pub/Sub broker
	broker := natsbackend.NewPubSubBroker(backend.Conn())
	defer broker.Close()

	// Create HTTP server with real-time support
	router := server.NewRouterWithRealtime(backend, cfg, broker, broker)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// Start server
	go func() {
		slog.Info("OJS server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start gRPC server
	grpcServer := grpc.NewServer()
	ojsgrpc.Register(grpcServer, backend)
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("ojs.v1.OJSService", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(grpcServer)

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
		if err != nil {
			slog.Error("failed to listen for gRPC", "port", cfg.GRPCPort, "error", err)
			os.Exit(1)
		}
		slog.Info("OJS gRPC server listening", "port", cfg.GRPCPort)
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	sched.Stop()
	grpcServer.GracefulStop()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	slog.Info("server stopped")
}
