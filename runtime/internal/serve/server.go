package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

const (
	defaultAddr            = ":7778"
	defaultReadTimeout     = 30 * time.Second
	defaultWriteTimeout    = 60 * time.Second
	defaultMaxRunAge       = time.Hour
	defaultWSPingInterval  = 30 * time.Second
	defaultEventBufferSize = 256
	gcInterval             = 60 * time.Second
)

// Server hosts the yawr HTTP/WebSocket/SSE API.
type Server struct {
	cfg             servepkg.ServerConfig
	engine          engine.Engine
	parser          parser.Parser
	planner         planner.Planner
	store           engine.RunStore
	registry        *RunRegistry
	bridge          *EventBridge
	broker          *PromptBroker
	mux             *http.ServeMux
	handler         http.Handler
	httpServer      *http.Server
	startedAt       time.Time
	wg              sync.WaitGroup
	stopOnce        sync.Once
	debugProfilesMu sync.Mutex
	delegations     *delegationsStore
}

// NewServer constructs a Server from configuration.
func NewServer(cfg servepkg.ServerConfig) (*Server, error) {
	return NewServerWithBroker(cfg, nil)
}

// NewServerWithBroker constructs a Server with an externally-supplied
// PromptBroker. Pass the same broker into BuildEngineConfig via
// WireOptions.PromptProviderOverride so that executors are wired to it
// before the engine is constructed.
func NewServerWithBroker(cfg servepkg.ServerConfig, broker *PromptBroker) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.MaxRunAge == 0 {
		cfg.MaxRunAge = defaultMaxRunAge
	}
	if cfg.WSPingInterval == 0 {
		cfg.WSPingInterval = defaultWSPingInterval
	}
	if cfg.EventBufferSize == 0 {
		cfg.EventBufferSize = defaultEventBufferSize
	}

	if cfg.Parser == nil {
		return nil, errors.New("serve: Parser is required")
	}
	if cfg.Planner == nil {
		return nil, errors.New("serve: Planner is required")
	}

	var eng engine.Engine
	// Construct the broker if the caller didn't supply one. Note: a broker
	// constructed here is too late to be wired into pre-built executors;
	// callers that want PromptBroker-backed choice/decision/collector
	// steps must build it before BuildEngineConfig and pass it in.
	if broker == nil {
		broker = NewPromptBroker(cfg.EventBufferSize)
	}
	if cfg.Engine != nil {
		eng = cfg.Engine
	} else {
		if cfg.EngineFactory == nil {
			return nil, errors.New("serve: EngineFactory is required when Engine is nil")
		}
		// If the caller didn't pre-wire the broker, install it as a
		// PromptProvider on EngineConfig as a best-effort fallback.
		if cfg.EngineConfig.PromptProvider == nil {
			cfg.EngineConfig.PromptProvider = broker
		}
		if err := cfg.EngineConfig.Validate(); err != nil {
			return nil, fmt.Errorf("serve: invalid engine config: %w", err)
		}
		eng = cfg.EngineFactory(cfg.EngineConfig)
	}

	s := &Server{
		cfg:         cfg,
		engine:      eng,
		parser:      cfg.Parser,
		planner:     cfg.Planner,
		store:       cfg.EngineConfig.Store,
		registry:    NewRunRegistry(),
		bridge:      NewEventBridge(cfg.EventBufferSize),
		broker:      broker,
		mux:         http.NewServeMux(),
		startedAt:   time.Now(),
		delegations: newDelegationsStore(),
	}

	s.routes()
	s.handler = s.withMiddleware(s.mux)
	s.httpServer = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /rpc", s.handleRPC)
	s.mux.HandleFunc("GET /ws", s.handleWS)
	s.mux.HandleFunc("GET /events", s.handleSSE)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /api/yawr/v1/runs/ingest", s.handleRunIngest)
	s.mux.HandleFunc("POST /api/yawr/v1/runs/{runID}/attachments/{sha256}", s.handleRunAttachment)
	s.mux.HandleFunc("GET /preview/document", s.handlePreviewDocument)
	s.mux.HandleFunc("GET /runs/{id}/document", s.handleRunDocument)
	s.mux.HandleFunc("GET /runs/{id}/state", s.handleRunState)
	// Interactive run wire (specs/runbook-interaction-wire-v1.md)
	s.mux.HandleFunc("POST /runs", s.handleRunsCreate)
	s.mux.HandleFunc("DELETE /runs/{id}", s.handleRunsDelete)
	s.mux.HandleFunc("GET /runs/{id}/interactions", s.handleInteractionsStream)
	s.mux.HandleFunc("POST /runs/{id}/interactions/{turnID}", s.handleInteractionAnswer)
	s.mux.HandleFunc("GET /debug-profiles", s.handleDebugProfilesList)
	s.mux.HandleFunc("PUT /debug-profiles/{id}", s.handleDebugProfilePut)
	// Static preview UI (React Flow client served from embedded asset).
	s.mux.HandleFunc("GET /preview/", s.handlePreviewIndex)
	s.mux.HandleFunc("GET /preview/assets/", s.handlePreviewAsset)
	// Delegation/invite endpoints (specs/runbook-delegation-v1.md)
	s.mux.HandleFunc("GET /api/yawr/v1/properties/{propertyID}/delegations", s.handleDelegationsList)
	s.mux.HandleFunc("PUT /api/yawr/v1/properties/{propertyID}/delegations", s.handleDelegationsPut)
	s.mux.HandleFunc("POST /api/yawr/v1/invites", s.handleInvitesCreate)
	s.mux.HandleFunc("POST /api/yawr/v1/invites/{token}/redeem", s.handleInvitesRedeem)
}

// Start launches the HTTP server and blocks until ctx is cancelled or the server exits.
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("serve: context is required")
	}
	s.startedAt = time.Now()

	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- s.httpServer.Serve(listener)
	}()

	gcCtx, gcCancel := context.WithCancel(context.Background())
	go s.runGC(gcCtx)

	select {
	case <-ctx.Done():
		gcCancel()
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Stop(stopCtx)
		err = <-serverErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err = <-serverErr:
		gcCancel()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.cancelActiveRuns()
		s.bridge.Close()
		if s.httpServer != nil {
			shutdownErr := s.httpServer.Shutdown(ctx)
			if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
				err = shutdownErr
			}
		}
		s.wg.Wait()
	})
	return err
}

func (s *Server) cancelActiveRuns() {
	entries := s.registry.List()
	for _, entry := range entries {
		if entry.Handle == nil {
			continue
		}
		state := entry.Handle.State().Status
		if isStoppedState(state) {
			continue
		}
		_ = entry.Handle.Cancel(context.Background(), "server stopping")
		if entry.Cancel != nil {
			entry.Cancel()
		}
		if s.broker != nil {
			s.broker.Unregister(entry.ID)
		}
		s.registry.WithEntry(entry.ID, func(e *RunEntry) {
			e.State = engine.RunStatusCancelled
			e.CompletedAt = time.Now()
		})
	}
}

func (s *Server) runGC(ctx context.Context) {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.registry.GC(s.cfg.MaxRunAge)
		}
	}
}

func isTerminalState(state engine.RunStatus) bool {
	switch state {
	case engine.RunStatusCompleted, engine.RunStatusFailed, engine.RunStatusCancelled:
		return true
	default:
		return false
	}
}

func isStoppedState(state engine.RunStatus) bool {
	return isTerminalState(state) || state == engine.RunStatusPausedAtBoundary || state == engine.RunStatusIndeterminate
}

func (s *Server) pumpEvents(ctx context.Context, entry *RunEntry) {
	defer s.wg.Done()
	if entry == nil || entry.Handle == nil {
		return
	}
	events := entry.Handle.Events()
	for {
		select {
		case <-ctx.Done():
			if s.broker != nil {
				s.broker.Unregister(entry.ID)
			}
			return
		case ev, ok := <-events:
			if !ok {
				s.syncRunEntryState(entry)
				if s.broker != nil {
					s.broker.Unregister(entry.ID)
				}
				return
			}
			s.bridge.Broadcast(toRunEvent(ev))
			if isTerminalRunEvent(ev.Kind) {
				s.syncRunEntryState(entry)
				if s.broker != nil {
					s.broker.Unregister(entry.ID)
				}
				return
			}
		}
	}
}

func isTerminalRunEvent(kind string) bool {
	switch kind {
	case "run/completed", "run/failed", "run/cancelled":
		return true
	default:
		return false
	}
}
