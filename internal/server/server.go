package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"

	commonapi "github.com/openjobspec/ojs-go-backend-common/api"
	commoncore "github.com/openjobspec/ojs-go-backend-common/core"
	"github.com/openjobspec/ojs-go-backend-common/events"
	ojsotel "github.com/openjobspec/ojs-go-backend-common/otel"
	"github.com/openjobspec/ojs-go-backend-common/registry"

	"github.com/openjobspec/ojs-backend-nats/internal/admin"
	"github.com/openjobspec/ojs-backend-nats/internal/api"
	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/metrics"
)

// NewRouter creates and configures the HTTP router with all OJS routes.
func NewRouter(backend core.Backend, cfgs ...Config) http.Handler {
	var cfg Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	return NewRouterWithRealtime(backend, &cfg, nil, nil)
}

// NewRouterWithRealtime creates and configures the HTTP router with all OJS routes
// including real-time SSE endpoints.
func NewRouterWithRealtime(backend core.Backend, cfg *Config, publisher core.EventPublisher, subscriber core.EventSubscriber) http.Handler {
	r := chi.NewRouter()

	registerMiddleware(r, cfg)

	// Prometheus metrics endpoint
	r.Handle("/metrics", promhttp.Handler())

	// Create handlers
	jobHandler := api.NewJobHandler(backend)
	workerHandler := api.NewWorkerHandler(backend)
	systemHandler := api.NewSystemHandler(backend)
	queueHandler := api.NewQueueHandler(backend)
	deadLetterHandler := api.NewDeadLetterHandler(backend)
	cronHandler := api.NewCronHandler(backend)
	workflowHandler := api.NewWorkflowHandler(backend)
	batchHandler := api.NewBatchHandler(backend)

	// Enable schema validation via in-memory registry
	schemaReg := commoncore.NewMemorySchemaRegistry()
	jobHandler.SetSchemaRegistry(schemaReg)

	// Wire event publishing and ensure a subscriber exists for real-time routes.
	subscriber = wireEvents(jobHandler, workerHandler, publisher, subscriber)

	registerSystemRoutes(r, systemHandler)
	registerJobRoutes(r, jobHandler, batchHandler)
	registerWorkerRoutes(r, workerHandler)
	registerQueueRoutes(r, queueHandler)
	registerDeadLetterRoutes(r, deadLetterHandler)
	registerCronRoutes(r, cronHandler)
	registerWorkflowRoutes(r, workflowHandler)
	registerSchemaRoutes(r)
	registerAdminRoutes(r, api.NewAdminHandler(backend))
	registerAdminUIRoutes(r)

	// API documentation (Swagger UI)
	commonapi.RegisterDocsRoutes(r, api.OpenAPISpec)

	registerRealtimeRoutes(r, backend, subscriber)

	return r
}

// registerMiddleware installs the shared middleware chain, including optional
// API-key authentication. It must run before any routes are registered.
func registerMiddleware(r chi.Router, cfg *Config) {
	r.Use(middleware.Recoverer)
	r.Use(tracingMiddleware)
	r.Use(metricsMiddleware)
	r.Use(api.OJSHeaders)
	r.Use(api.RequestLogger)
	r.Use(api.LimitBody)
	r.Use(api.ValidateContentType)

	if cfg.APIKey != "" {
		r.Use(api.KeyAuth(cfg.APIKey, "/metrics", "/ojs/v1/health"))
	}
}

// wireEvents connects the job/worker handlers to an event publisher and returns
// the subscriber to use for real-time routes, bootstrapping an in-memory event
// bus when the backend provides no native pub/sub.
func wireEvents(jobHandler *api.JobHandler, workerHandler *api.WorkerHandler, publisher core.EventPublisher, subscriber core.EventSubscriber) core.EventSubscriber {
	if publisher != nil {
		jobHandler.SetEventPublisher(publisher)
		workerHandler.SetEventPublisher(publisher)
	}

	if subscriber == nil {
		bus := events.NewBus(events.BusConfig{BufferSize: 256})
		if publisher == nil {
			jobHandler.SetEventPublisher(bus)
			workerHandler.SetEventPublisher(bus)
		}
		subscriber = bus
	}

	return subscriber
}

func registerSystemRoutes(r chi.Router, h *api.SystemHandler) {
	r.Get("/ojs/manifest", h.Manifest)
	r.Get("/ojs/v1/health", h.Health)
	r.Get("/healthz", h.Healthz)
	r.Get("/readyz", h.Readyz)
}

func registerJobRoutes(r chi.Router, jobHandler *api.JobHandler, batchHandler *api.BatchHandler) {
	r.Post("/ojs/v1/jobs", jobHandler.Create)
	r.Get("/ojs/v1/jobs/{id}", jobHandler.Get)
	r.Delete("/ojs/v1/jobs/{id}", jobHandler.Cancel)

	// Batch enqueue
	r.Post("/ojs/v1/jobs/batch", batchHandler.Create)
}

func registerWorkerRoutes(r chi.Router, h *api.WorkerHandler) {
	r.Post("/ojs/v1/workers/fetch", h.Fetch)
	r.Post("/ojs/v1/workers/ack", h.Ack)
	r.Post("/ojs/v1/workers/nack", h.Nack)
	r.Post("/ojs/v1/workers/heartbeat", h.Heartbeat)
}

func registerQueueRoutes(r chi.Router, h *api.QueueHandler) {
	r.Get("/ojs/v1/queues", h.List)
	r.Get("/ojs/v1/queues/{name}/stats", h.Stats)
	r.Post("/ojs/v1/queues/{name}/pause", h.Pause)
	r.Post("/ojs/v1/queues/{name}/resume", h.Resume)
}

func registerDeadLetterRoutes(r chi.Router, h *api.DeadLetterHandler) {
	r.Get("/ojs/v1/dead-letter", h.List)
	r.Post("/ojs/v1/dead-letter/{id}/retry", h.Retry)
	r.Delete("/ojs/v1/dead-letter/{id}", h.Delete)
}

func registerCronRoutes(r chi.Router, h *api.CronHandler) {
	r.Get("/ojs/v1/cron", h.List)
	r.Post("/ojs/v1/cron", h.Register)
	r.Delete("/ojs/v1/cron/{name}", h.Delete)
}

func registerWorkflowRoutes(r chi.Router, h *api.WorkflowHandler) {
	r.Post("/ojs/v1/workflows", h.Create)
	r.Get("/ojs/v1/workflows/{id}", h.Get)
	r.Delete("/ojs/v1/workflows/{id}", h.Cancel)
}

func registerSchemaRoutes(r chi.Router) {
	schemaRegistry := registry.NewSchemaRegistry()
	schemaHandler := registry.NewSchemaHandler(schemaRegistry)
	r.Post("/ojs/v1/schemas", schemaHandler.HandleRegister)
	r.Get("/ojs/v1/schemas/{jobType}", schemaHandler.HandleGetLatest)
	r.Get("/ojs/v1/schemas/{jobType}/versions", schemaHandler.HandleListVersions)
	r.Get("/ojs/v1/schemas/{jobType}/versions/{version}", schemaHandler.HandleGetVersion)
	r.Post("/ojs/v1/schemas/{jobType}/validate", schemaHandler.HandleValidate)
	r.Put("/ojs/v1/schemas/{jobType}/compatibility", schemaHandler.HandleSetCompatibility)
	r.Delete("/ojs/v1/schemas/{jobType}", schemaHandler.HandleDelete)
	r.Delete("/ojs/v1/schemas/{jobType}/versions/{version}", schemaHandler.HandleDelete)
}

func registerAdminRoutes(r chi.Router, h *api.AdminHandler) {
	r.Get("/ojs/v1/admin/stats", h.Stats)
	r.Get("/ojs/v1/admin/queues", h.ListQueues)
	r.Get("/ojs/v1/admin/queues/{name}", h.GetQueue)
	r.Post("/ojs/v1/admin/queues/{name}/pause", h.PauseQueue)
	r.Post("/ojs/v1/admin/queues/{name}/resume", h.ResumeQueue)
	r.Get("/ojs/v1/admin/jobs", h.ListJobs)
	r.Get("/ojs/v1/admin/jobs/{id}", h.GetJob)
	r.Post("/ojs/v1/admin/jobs/{id}/retry", h.RetryJob)
	r.Post("/ojs/v1/admin/jobs/{id}/cancel", h.CancelJob)
	r.Post("/ojs/v1/admin/jobs/bulk/retry", h.BulkRetry)
	r.Get("/ojs/v1/admin/workers", h.ListWorkers)
	r.Post("/ojs/v1/admin/workers/{id}/quiet", h.QuietWorker)
	r.Get("/ojs/v1/admin/dead-letter", h.ListDeadLetter)
	r.Get("/ojs/v1/admin/dead-letter/stats", h.DeadLetterStats)
	r.Post("/ojs/v1/admin/dead-letter/{id}/retry", h.RetryDeadLetter)
	r.Delete("/ojs/v1/admin/dead-letter/{id}", h.DeleteDeadLetter)
	r.Post("/ojs/v1/admin/dead-letter/retry", h.BulkRetryDeadLetter)
}

func registerAdminUIRoutes(r chi.Router) {
	r.Handle("/ojs/admin", http.RedirectHandler("/ojs/admin/", http.StatusMovedPermanently))
	r.Mount("/ojs/admin/", http.StripPrefix("/ojs/admin/", admin.Handler()))
}

// registerRealtimeRoutes wires the SSE, native WebSocket, and WebSocket-bridge
// endpoints that stream job and queue events to clients.
func registerRealtimeRoutes(r chi.Router, backend core.Backend, subscriber core.EventSubscriber) {
	// Real-time SSE endpoints (always available via event bus)
	sseHandler := api.NewSSEHandler(backend, subscriber)
	r.Get("/ojs/v1/jobs/{id}/events", sseHandler.JobEvents)
	r.Get("/ojs/v1/queues/{name}/events", sseHandler.QueueEvents)

	// Native WebSocket endpoint
	nativeWSHandler := api.NewWSHandler(backend, subscriber)
	r.Get("/ojs/v1/ws", nativeWSHandler.Handle)

	// WebSocket bridge endpoints (SSE-based WS alternative)
	wsBridgeHandler := api.NewWSBridgeHandler(subscriber)
	r.Get("/ojs/v1/ws/connect", wsBridgeHandler.Connect)
	r.Post("/ojs/v1/ws/subscribe", wsBridgeHandler.Subscribe)
	r.Post("/ojs/v1/ws/unsubscribe", wsBridgeHandler.Unsubscribe)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped, ww := api.NewStatusResponseWriter(w)
		next.ServeHTTP(wrapped, r)
		duration := time.Since(start).Seconds()
		path := metricRoutePattern(r)
		statusCode := fmt.Sprintf("%d", ww.Status())
		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, path, statusCode).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, path, statusCode).Observe(duration)
	})
}

// tracingMiddleware mirrors the shared OTel instrumentation without hiding
// streaming and connection-upgrade interfaces from SSE and WebSocket handlers.
func tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := ojsotel.Tracer().Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				attribute.String("url.path", r.URL.Path),
				semconv.ServerAddress(r.Host),
			),
		)
		defer span.End()

		wrapped, sw := api.NewStatusResponseWriter(w)
		next.ServeHTTP(wrapped, r.WithContext(ctx))
		span.SetAttributes(semconv.HTTPResponseStatusCode(sw.Status()))
	})
}

func metricRoutePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}
