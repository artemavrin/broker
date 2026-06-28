package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// doJSON issues a request with an optional bearer token and JSON body, and
// decodes the response into out (when non-nil). It returns the status code.
func doJSON(t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode
}

// TestRevokeBlocksAuth provisions a receiver over HTTP, confirms its secret
// yields a token, then revokes it and confirms the same secret is rejected.
func TestRevokeBlocksAuth(t *testing.T) {
	e := newEnv(t)
	_, itok := e.newInitiator(t)
	base := e.server.URL

	var created struct {
		ReceiverID     string `json:"receiver_id"`
		ReceiverSecret string `json:"receiver_secret"`
	}
	if code := doJSON(t, "POST", base+"/v1/receivers", itok, nil, &created); code != 200 {
		t.Fatalf("create receiver: status %d", code)
	}

	// The secret works before revocation.
	if code := doJSON(t, "POST", base+"/v1/auth/token", "",
		map[string]string{"secret": created.ReceiverSecret}, nil); code != 200 {
		t.Fatalf("expected 200 before revoke, got %d", code)
	}

	if code := doJSON(t, "DELETE", base+"/v1/receivers/"+created.ReceiverID, itok, nil, nil); code != 204 {
		t.Fatalf("delete receiver: status %d", code)
	}

	// After revocation the same secret is rejected.
	if code := doJSON(t, "POST", base+"/v1/auth/token", "",
		map[string]string{"secret": created.ReceiverSecret}, nil); code != 401 {
		t.Fatalf("expected 401 after revoke, got %d", code)
	}
}
