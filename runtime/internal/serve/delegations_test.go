package serve

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDelegations_PutThenList(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.handler)
	defer ts.Close()

	body := `{"delegations":[{"Delegate":{"Name":"Carlos"}}]}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/yawr/v1/properties/casa-santiago/delegations",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}

	got, err := http.Get(ts.URL + "/api/yawr/v1/properties/casa-santiago/delegations")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", got.StatusCode)
	}
	raw, _ := io.ReadAll(got.Body)
	if !bytes.Contains(raw, []byte(`"Carlos"`)) {
		t.Fatalf("response missing delegation: %s", raw)
	}
}

func TestInvites_CreateAndRedeem(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.handler)
	defer ts.Close()

	create := `{"propertyID":"casa-santiago","delegation":{"Delegate":{"Name":"Lucia"}},"ttlHours":24}`
	resp, err := http.Post(ts.URL+"/api/yawr/v1/invites",
		"application/json", strings.NewReader(create))
	if err != nil {
		t.Fatalf("POST invite: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("invite create status = %d: %s", resp.StatusCode, raw)
	}
	var ci createInviteResponse
	if err := json.NewDecoder(resp.Body).Decode(&ci); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ci.Token == "" || !strings.HasPrefix(ci.URL, "https://yawrhome.app/invite/") {
		t.Fatalf("bad invite resp: %+v", ci)
	}

	r2, err := http.Post(ts.URL+"/api/yawr/v1/invites/"+ci.Token+"/redeem", "application/json", nil)
	if err != nil {
		t.Fatalf("POST redeem: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(r2.Body)
		t.Fatalf("redeem status = %d: %s", r2.StatusCode, raw)
	}
	raw, _ := io.ReadAll(r2.Body)
	if !bytes.Contains(raw, []byte(`"Lucia"`)) {
		t.Fatalf("redeem missing delegation: %s", raw)
	}

	// second redeem should fail
	r3, _ := http.Post(ts.URL+"/api/yawr/v1/invites/"+ci.Token+"/redeem", "application/json", nil)
	defer r3.Body.Close()
	if r3.StatusCode == http.StatusOK {
		t.Fatalf("second redeem should fail, got 200")
	}
}
