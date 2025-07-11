package apiv1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/inngest/inngest/pkg/api/apiv1/apiv1auth"
	"github.com/inngest/inngest/pkg/cqrs"
	"github.com/inngest/inngest/pkg/execution"
	"github.com/inngest/inngest/pkg/execution/queue"
	"github.com/inngest/inngest/pkg/execution/realtime"
	"github.com/inngest/inngest/pkg/execution/state/redis_state"
	"github.com/inngest/inngest/pkg/headers"
)

// Opts represents options for the APIv1 router.
type Opts struct {
	// AuthMiddleware authenticates the incoming API request.
	AuthMiddleware func(http.Handler) http.Handler
	// CachingMiddleware caches API responses, if the handler specifies
	// a max-age.
	CachingMiddleware CachingMiddleware
	// WorkspaceFinder returns the authenticated workspace given the current context.
	AuthFinder apiv1auth.AuthFinder
	// Executor is required to cancel and manage function executions.
	Executor execution.Executor
	// FunctionReader reads functions from a backing store.
	FunctionReader cqrs.FunctionReader
	// FunctionRunReader reads function runs, history, etc. from backing storage
	FunctionRunReader cqrs.APIV1FunctionRunReader
	// JobQueueReader reads information around a function run's job queues.
	JobQueueReader queue.JobQueueReader
	// CancellationReadWriter reads and writes cancellations to/from a backing store.
	CancellationReadWriter cqrs.CancellationReadWriter
	// QueueShardSelector determines the queue shard to use
	QueueShardSelector redis_state.ShardSelector
	// Broadcaster is used to handle realtime via APIv1
	Broadcaster realtime.Broadcaster
	// RealtimeJWTSecret is the realtime JWT secret for the V1 API
	RealtimeJWTSecret []byte
	// TraceReader reads traces from a backing store.
	TraceReader cqrs.TraceReader
	// MetricsMiddleware is used to instrument the APIv1 endpoints.
	MetricsMiddleware MetricsMiddleware
}

// AddRoutes adds a new API handler to the given router.
func AddRoutes(r chi.Router, o Opts) http.Handler {
	if o.AuthFinder == nil {
		o.AuthFinder = apiv1auth.NilAuthFinder
	}

	// Create the HTTP implementation, which wraps the handler.  We do ths to code
	// share and split the HTTP concerns from the actual logic, eg. to share to GQL.
	impl := &API{opts: o}

	instance := &router{
		Router: r,
		API:    impl,
	}

	instance.setup()
	return instance
}

type API struct {
	opts Opts
}

type router struct {
	*API
	chi.Router
}

func (a *router) setup() {
	a.Group(func(r chi.Router) {
		r.Use(middleware.Recoverer)

		if len(a.opts.RealtimeJWTSecret) > 0 {
			// Only enable realtime if secrets are set.
			r.Group(func(r chi.Router) {
				rt := realtime.NewAPI(realtime.APIOpts{
					JWTSecret:      a.opts.RealtimeJWTSecret,
					Broadcaster:    a.opts.Broadcaster,
					AuthMiddleware: a.opts.AuthMiddleware,
					AuthFinder:     a.opts.AuthFinder,
				})
				r.Mount("/", rt)
			})
		}

		r.Group(func(r chi.Router) {
			if a.opts.AuthMiddleware != nil {
				r.Use(a.opts.AuthMiddleware)
			}

			if a.opts.CachingMiddleware != nil {
				r.Use(a.opts.CachingMiddleware.Middleware)
			}

			if a.opts.MetricsMiddleware != nil {
				r.Use(a.opts.MetricsMiddleware.Middleware)
			}

			r.Use(headers.ContentTypeJsonResponse())

			r.Post("/signals", a.receiveSignal)

			r.Get("/events", a.getEvents)
			r.Get("/events/{eventID}", a.getEvent)
			r.Get("/events/{eventID}/runs", a.getEventRuns)
			r.Get("/runs/{runID}", a.GetFunctionRun)
			r.Delete("/runs/{runID}", a.cancelFunctionRun)
			r.Get("/runs/{runID}/jobs", a.GetFunctionRunJobs)

			r.Get("/apps/{appName}/functions", a.GetAppFunctions) // Returns an app and all of its functions.

			r.Post("/cancellations", a.createCancellation)
			r.Get("/cancellations", a.getCancellations)
			r.Delete("/cancellations/{id}", a.deleteCancellation)

			r.Get("/prom/{env}", a.promScrape)

			r.Post("/traces/userland", a.traces)
		})
	})
}

func WriteResponse[T any](w http.ResponseWriter, data T) error {
	return WriteCachedResponse(w, data, 0)
}

func WriteCachedResponse[T any](w http.ResponseWriter, data T, cachePeriod time.Duration) error {
	resp := Response[T]{
		Data: data,
		Metadata: ResponseMetadata{
			FetchedAt: time.Now(),
		},
	}

	if cachePeriod.Seconds() > 0 {
		cachedUntil := time.Now().Add(cachePeriod)
		resp.Metadata.CachedUntil = &cachedUntil
		// Set a max-age header if the response is cacheable.  This instructs
		// our caching middleware to cache the result for this period of time.
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(cachePeriod.Seconds())))
	}

	return json.NewEncoder(w).Encode(resp)
}

// Response represents
type Response[T any] struct {
	Data     T                `json:"data"`
	Metadata ResponseMetadata `json:"metadata"`
}

// ResponseMetadata represents metadata regarding the response.
type ResponseMetadata struct {
	FetchedAt   time.Time  `json:"fetched_at,omitempty"`
	CachedUntil *time.Time `json:"cached_until,omitempty"`
}
