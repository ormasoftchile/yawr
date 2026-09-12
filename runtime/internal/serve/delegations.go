package serve

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// delegationsStore is an in-memory store of delegations and the
// invite tokens that resolve to them. The intent is small but
// honest: a real deployment would persist these alongside runs and
// scope them to a property + owner identity. Today they're keyed
// by property id (a single namespace) and survive only as long as
// the process.
type delegationsStore struct {
	mu      sync.RWMutex
	byProp  map[string][]json.RawMessage
	invites map[string]inviteRecord
}

type inviteRecord struct {
	PropertyID string          `json:"propertyID"`
	Delegation json.RawMessage `json:"delegation"`
	CreatedAt  time.Time       `json:"createdAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Redeemed   bool            `json:"redeemed"`
}

func newDelegationsStore() *delegationsStore {
	return &delegationsStore{
		byProp:  map[string][]json.RawMessage{},
		invites: map[string]inviteRecord{},
	}
}

func (d *delegationsStore) list(propertyID string) []json.RawMessage {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]json.RawMessage, len(d.byProp[propertyID]))
	copy(out, d.byProp[propertyID])
	return out
}

func (d *delegationsStore) replace(propertyID string, items []json.RawMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byProp[propertyID] = items
}

func (d *delegationsStore) issueInvite(propertyID string, delegation json.RawMessage, ttl time.Duration) (string, inviteRecord, error) {
	token, err := randomToken()
	if err != nil {
		return "", inviteRecord{}, err
	}
	rec := inviteRecord{
		PropertyID: propertyID,
		Delegation: delegation,
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(ttl),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invites[token] = rec
	return token, rec, nil
}

func (d *delegationsStore) redeem(token string) (inviteRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.invites[token]
	if !ok {
		return inviteRecord{}, errors.New("unknown invite token")
	}
	if time.Now().After(rec.ExpiresAt) {
		return inviteRecord{}, errors.New("invite token expired")
	}
	if rec.Redeemed {
		return inviteRecord{}, errors.New("invite token already redeemed")
	}
	rec.Redeemed = true
	d.invites[token] = rec
	// Append the redeemed delegation onto the property store so
	// owner-side polling sees it.
	d.byProp[rec.PropertyID] = append(d.byProp[rec.PropertyID], rec.Delegation)
	return rec, nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HTTP handlers

// putDelegationsRequest mirrors the iOS SyncClient's outgoing JSON.
// "delegations" is forwarded verbatim — the server does not parse the
// shape, only stores and returns it.
type putDelegationsRequest struct {
	Delegations []json.RawMessage `json:"delegations"`
}

func (s *Server) handleDelegationsList(w http.ResponseWriter, r *http.Request) {
	prop := r.PathValue("propertyID")
	if prop == "" {
		http.Error(w, "propertyID is required", http.StatusBadRequest)
		return
	}
	out := struct {
		Delegations []json.RawMessage `json:"delegations"`
	}{Delegations: s.delegations.list(prop)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleDelegationsPut(w http.ResponseWriter, r *http.Request) {
	prop := r.PathValue("propertyID")
	if prop == "" {
		http.Error(w, "propertyID is required", http.StatusBadRequest)
		return
	}
	var req putDelegationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	s.delegations.replace(prop, req.Delegations)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"stored": len(req.Delegations)})
}

type createInviteRequest struct {
	PropertyID string          `json:"propertyID"`
	Delegation json.RawMessage `json:"delegation"`
	TTLHours   int             `json:"ttlHours"`
}

type createInviteResponse struct {
	Token     string `json:"token"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
}

func (s *Server) handleInvitesCreate(w http.ResponseWriter, r *http.Request) {
	var req createInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.PropertyID) == "" {
		http.Error(w, "propertyID is required", http.StatusBadRequest)
		return
	}
	if len(req.Delegation) == 0 {
		http.Error(w, "delegation is required", http.StatusBadRequest)
		return
	}
	ttl := time.Duration(req.TTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	token, rec, err := s.delegations.issueInvite(req.PropertyID, req.Delegation, ttl)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not issue invite: %v", err), http.StatusInternalServerError)
		return
	}
	resp := createInviteResponse{
		Token:     token,
		URL:       "https://yawrhome.app/invite/" + token,
		ExpiresAt: rec.ExpiresAt.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

type redeemInviteResponse struct {
	PropertyID string          `json:"propertyID"`
	Delegation json.RawMessage `json:"delegation"`
}

func (s *Server) handleInvitesRedeem(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	rec, err := s.delegations.redeem(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := redeemInviteResponse{
		PropertyID: rec.PropertyID,
		Delegation: rec.Delegation,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
