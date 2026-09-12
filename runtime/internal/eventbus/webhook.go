package eventbus

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"time"

	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

// WebhookListener receives inbound events over HTTP.
type WebhookListener struct {
	server     *http.Server
	dispatcher *Dispatcher
}

// NewWebhookListener constructs a WebhookListener.
func NewWebhookListener(addr string, dispatcher *Dispatcher) *WebhookListener {
	mux := http.NewServeMux()
	listener := &WebhookListener{
		server: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
		dispatcher: dispatcher,
	}
	mux.HandleFunc("POST /events/inbound", listener.handleInbound)
	return listener
}

// Start begins listening. It returns when the server shuts down.
func (w *WebhookListener) Start(ctx context.Context) error {
	if w == nil || w.server == nil {
		return errors.New("eventbus: webhook server not configured")
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.server.Shutdown(stopCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Stop gracefully shuts down the listener.
func (w *WebhookListener) Stop(ctx context.Context) error {
	if w == nil || w.server == nil {
		return nil
	}
	return w.server.Shutdown(ctx)
}

func (w *WebhookListener) handleInbound(wr http.ResponseWriter, r *http.Request) {
	if w == nil || w.dispatcher == nil {
		http.Error(wr, "dispatcher unavailable", http.StatusServiceUnavailable)
		return
	}

	body, err := readBody(r)
	if err != nil {
		http.Error(wr, "invalid body", http.StatusBadRequest)
		return
	}
	if !verifySignature(body, r.Header.Get("X-Yawr-Signature")) {
		http.Error(wr, "unauthorized", http.StatusUnauthorized)
		return
	}

	var payload struct {
		RunID   string         `json:"run_id"`
		EventID string         `json:"event_id"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(wr, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.EventID == "" {
		http.Error(wr, "missing event_id", http.StatusBadRequest)
		return
	}

	_ = w.dispatcher.Dispatch(eventbuspkg.InboundEvent{
		EventID: payload.EventID,
		Source:  "webhook",
		Channel: payload.EventID,
		Payload: payload.Payload,
	})
	wr.WriteHeader(http.StatusOK)
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func verifySignature(body []byte, signature string) bool {
	key := os.Getenv("YAWR_WEBHOOK_KEY")
	if key == "" {
		return true
	}
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
