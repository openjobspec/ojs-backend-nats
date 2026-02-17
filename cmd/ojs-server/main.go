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

	// Create HTTP server
	router := server.NewRouter(backend, cfg)
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
