package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	enginePkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/asciigraph"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/mermaid"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/prose"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

// handlePreviewDocument serves the design-time graphjson for a runbook
// path. GET /preview/document?runbookPath=...&recurse=true
func (s *Server) handlePreviewDocument(w http.ResponseWriter, r *http.Request) {
	rbPath := strings.TrimSpace(r.URL.Query().Get("runbookPath"))
	if rbPath == "" {
		http.Error(w, "runbookPath query parameter is required", http.StatusBadRequest)
		return
	}
	recurse := r.URL.Query().Get("recurse") == "true"

	doc, err := s.buildPreviewDocument(r.Context(), rbPath, recurse)
	if err != nil {
		http.Error(w, fmt.Sprintf("build document: %v", err), http.StatusBadRequest)
		return
	}
	writeDocument(w, r, doc, r.URL.Query().Get("format"))
}

// writeDocument renders the document in the requested format and writes
// it to w with appropriate Content-Type. Supported formats:
//
//	graphjson (default) — application/json
//	prose               — text/markdown; charset=utf-8
//	mermaid             — text/plain; charset=utf-8
//	asciigraph          — text/plain; charset=utf-8
//
// Documents are cacheable by ETag but require revalidation. This protects
// the server when a faulty preview client repeatedly asks for an unchanged
// terminal document: conditional requests return 304 with no body.
func writeDocument(w http.ResponseWriter, r *http.Request, doc *graphdoc.Document, format string) {
	if doc != nil {
		doc = doc.ForExpressionRendering()
	}
	etag := documentETag(doc, format)
	if etag != "" {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r != nil && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	switch format {
	case "prose":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(prose.Render(doc)))
	case "mermaid":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(mermaid.Render(doc)))
	case "asciigraph":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(asciigraph.Render(doc)))
	default: // graphjson
		out, err := graphjson.Render(doc)
		if err != nil {
			http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}
}

func documentETag(doc *graphdoc.Document, format string) string {
	if doc == nil || doc.Hash == "" {
		return ""
	}
	if format == "" {
		format = "graphjson"
	}
	suffix := ""
	if doc.PresentationState != nil {
		suffix = ":" + strconv.FormatUint(doc.PresentationState.CheckpointSequence, 10)
	}
	return strconv.Quote(format + ":" + doc.Hash + suffix)
}

// handleRunDocument serves the graphjson for an active or completed run.
// GET /runs/{id}/document
//
// The document is rendered with frontier-based expansion: includes the
// runtime has actually entered are inlined; everything else stays
// opaque. This keeps large orchestrator runbooks fast to render while
// still letting the UI focus on awaiting-input steps inside loaded
// sub-runbooks.
//
// ?recurse=true forces full inlining for callers that explicitly want
// the whole resolved tree (e.g., post-mortem inspection).
func (s *Server) handleRunDocument(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	rateChecked := false
	if store, ok := s.store.(enginePkg.DurableRunStore); ok {
		if _, registered := s.registry.Get(runID); registered && !s.registry.AllowPreviewRead(runID, time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "preview read rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		rateChecked = true
		doc, err := presentationview.Inspect(r.Context(), store, runID)
		if err != nil {
			_, registered := s.registry.Get(runID)
			if !registered || !errors.Is(err, os.ErrNotExist) {
				http.Error(w, "saved run document unavailable", http.StatusNotFound)
				return
			}
		} else {
			writeDocument(w, r, doc, r.URL.Query().Get("format"))
			return
		}
	}
	entry, ok := s.registry.Get(runID)
	if !ok {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if !rateChecked && !s.registry.AllowPreviewRead(runID, time.Now()) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "preview read rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	var (
		doc *graphdoc.Document
		err error
	)
	if r.URL.Query().Get("recurse") == "true" {
		doc, err = s.buildPreviewDocument(r.Context(), entry.RunbookPath, true)
	} else {
		active := s.activeIncludeIDs(runID)
		doc, err = s.buildRunDocument(r.Context(), entry.RunbookPath, active)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("build document: %v", err), http.StatusBadRequest)
		return
	}
	writeDocument(w, r, doc, r.URL.Query().Get("format"))
}

// activeIncludeIDs replays the run's events to find every step ID the
// engine has touched (status != pending). Any of these IDs that turn
// out to be includes will be expanded by the document builder.
func (s *Server) activeIncludeIDs(runID string) map[string]bool {
	state := runstate.New()
	for _, ev := range s.bridge.Replay(runID, 0) {
		state.Apply(runEventToEngineEvent(ev))
	}
	snap := state.Snapshot()
	active := make(map[string]bool, len(snap.Nodes))
	for id, n := range snap.Nodes {
		if n.Status != "" && n.Status != runstate.StatusPending {
			active[id] = true
		}
	}
	return active
}

// buildRunDocument is like buildPreviewDocument but recurses only into
// the include step IDs in `active`.
func (s *Server) buildRunDocument(ctx context.Context, path string, active map[string]bool) (*graphdoc.Document, error) {
	rb, err := s.parser.Parse(ctx, path)
	if err != nil {
		return nil, err
	}
	loader := &serveLoader{p: s.parser}
	return (&graphdoc.Builder{
		Loader:            loader,
		RecurseIncludeIDs: active,
	}).Build(ctx, rb)
}

// handleRunState streams runtime state for a run as NDJSON-over-SSE.
// GET /runs/{id}/state
//
// The first frame is a full state snapshot reconstructed from the event
// replay buffer; subsequent frames are sent on every new engine.Event.
func (s *Server) handleRunState(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, ok := s.registry.Get(runID); !ok {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	clearSSEDeadlines(w)
	w.WriteHeader(http.StatusOK)

	state := runstate.New()
	// Replay history first so the client gets a coherent initial state.
	for _, ev := range s.bridge.Replay(runID, 0) {
		state.Apply(runEventToEngineEvent(ev))
	}
	if err := writeStateFrame(w, state.Snapshot()); err != nil {
		return
	}
	flusher.Flush()

	sub := s.bridge.Subscribe(runID, s.cfg.EventBufferSize)
	defer s.bridge.Unsubscribe(sub)

	hbTicker := time.NewTicker(sseHeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hbTicker.C:
			if err := writeSSEHeartbeat(w); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			state.Apply(runEventToEngineEvent(ev))
			if err := writeStateFrame(w, state.Snapshot()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// buildPreviewDocument parses the given runbook and returns the graphdoc.
func (s *Server) buildPreviewDocument(ctx context.Context, path string, recurse bool) (*graphdoc.Document, error) {
	rb, err := s.parser.Parse(ctx, path)
	if err != nil {
		return nil, err
	}
	loader := &serveLoader{p: s.parser}
	doc, err := (&graphdoc.Builder{Loader: loader, Recurse: recurse}).Build(ctx, rb)
	if err != nil {
		return nil, err
	}
	root := s.cfg.WorkspaceRoot
	if root == "" {
		root, _ = os.Getwd()
	}
	abs, _ := filepath.Abs(path)
	packageMap := s.cfg.PackageMapPath
	if packageMap != "" {
		packageMap, _ = filepath.Abs(packageMap)
	}
	graphdoc.AddCurrentPresentation(doc, presentation.Context{ProjectRoot: root, EntrypointPath: abs, PackageMapPath: packageMap})
	return doc, nil
}

// serveLoader adapts the server's parser to flowwalk.Loader.
type serveLoader struct{ p parserPkg.Parser }

func (l *serveLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return l.p.Parse(ctx, path)
}

// runEventToEngineEvent decodes a wire RunEvent payload into the
// engine.Event shape that runstate.Apply expects.
func runEventToEngineEvent(ev servepkg.RunEvent) enginePkg.Event {
	return enginePkg.Event{
		Kind:      ev.Type,
		Sequence:  ev.Sequence,
		Timestamp: ev.TS,
		RunID:     ev.RunID,
		Payload:   ev.Payload,
	}
}

// writeStateFrame writes a single NDJSON-over-SSE frame containing the
// state snapshot. The event type is "state" so clients can distinguish
// these from raw events on /events.
func writeStateFrame(w http.ResponseWriter, snap *runstate.State) error {
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: state\nid: %d\ndata: %s\n\n", snap.Sequence, body); err != nil {
		return err
	}
	return nil
}
