package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

// sseHeartbeatInterval is how often SSE handlers emit a comment frame
// to keep proxies and clients from declaring an idle connection dead.
// EventSource clients ignore comment frames silently. Exposed as a var
// so tests can shrink it.
var sseHeartbeatInterval = 15 * time.Second

// writeSSEHeartbeat emits an SSE comment frame. Clients treat it as a
// no-op; intermediaries treat it as activity.
func writeSSEHeartbeat(w http.ResponseWriter) error {
	_, err := fmt.Fprint(w, ": hb\n\n")
	return err
}

// clearSSEDeadlines disables the per-response read/write deadlines that the
// outer http.Server applies to ordinary requests. SSE streams are long-lived
// and would otherwise be cut off mid-chunk when the global WriteTimeout
// elapses, surfacing in browsers as net::ERR_INCOMPLETE_CHUNKED_ENCODING.
// SetWriteDeadline/SetReadDeadline returning ErrNotSupported (e.g. on test
// recorders) is safe to ignore.
func clearSSEDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
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
	flusher.Flush()

	runID := r.URL.Query().Get("runID")
	sub := s.bridge.Subscribe(runID, s.cfg.EventBufferSize)
	defer s.bridge.Unsubscribe(sub)

	hbTicker := time.NewTicker(sseHeartbeatInterval)
	defer hbTicker.Stop()

	lastEventID := parseLastEventID(r)
	if lastEventID > 0 {
		for _, ev := range s.bridge.Replay(runID, lastEventID) {
			if err := writeSSE(w, ev); err != nil {
				return
			}
		}
		flusher.Flush()
	}

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
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev servepkg.RunEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", ev.Type, ev.Sequence, data); err != nil {
		return err
	}
	return nil
}

func parseLastEventID(r *http.Request) int64 {
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if val, err := strconv.ParseInt(id, 10, 64); err == nil {
			return val
		}
	}
	return 0
}
