package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "closing")

	runID := r.URL.Query().Get("runID")
	sub := s.bridge.Subscribe(runID, s.cfg.EventBufferSize)
	defer s.bridge.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	pingInterval := s.cfg.WSPingInterval
	if pingInterval <= 0 {
		pingInterval = defaultWSPingInterval
	}
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
				return
			}
		case <-pingTicker.C:
			pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancelPing()
			if err != nil {
				return
			}
		}
	}
}
