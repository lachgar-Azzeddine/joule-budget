package credentials

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeJWT struct{ audience string }

func (f *fakeJWT) FetchJWT(_ context.Context, audience string) (string, error) {
	f.audience = audience
	return "svid-jwt", nil
}

// fakeBao accepts one role/JWT pair and serves one secret.
type fakeBao struct {
	mu      sync.Mutex
	logins  int
	revoked int
}

func (f *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/v1/auth/jwt/login":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)

		if body["role"] != "joule-budget" || body["jwt"] != "svid-jwt" {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		f.logins++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "t1"}})
	case "/v1/secret/data/joule-system/proxmox":
		if r.Header.Get("X-Vault-Token") != "t1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"token_id": "id", "token_secret": "s"}}})
	case "/v1/auth/token/revoke-self":
		f.revoked++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestSecretLogsInWithSPIFFEAndCaches(t *testing.T) {
	bao := &fakeBao{}
	srv := httptest.NewServer(bao)
	t.Cleanup(srv.Close)

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	jwt := &fakeJWT{}
	o := &OpenBao{Address: srv.URL, Role: "joule-budget", Audience: "openbao", Path: "joule-system/proxmox",
		TTL: 10 * time.Minute, JWT: jwt, now: func() time.Time { return now }}

	for range 3 {
		s, err := o.Secret(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		if s["token_id"] != "id" || s["token_secret"] != "s" {
			t.Fatalf("secret = %v", s)
		}
	}

	if jwt.audience != "openbao" {
		t.Errorf("JWT-SVID audience = %q", jwt.audience)
	}

	if bao.logins != 1 || bao.revoked != 1 {
		t.Errorf("logins=%d revoked=%d, want 1 and 1 (cached, token revoked after use)", bao.logins, bao.revoked)
	}

	now = now.Add(11 * time.Minute)

	if _, err := o.Secret(context.Background()); err != nil {
		t.Fatal(err)
	}

	if bao.logins != 2 {
		t.Errorf("logins=%d after the TTL, want 2", bao.logins)
	}
}

func TestSecretWrongRoleIsRefused(t *testing.T) {
	srv := httptest.NewServer(&fakeBao{})
	t.Cleanup(srv.Close)

	o := &OpenBao{Address: srv.URL, Role: "someone-else", Audience: "openbao", Path: "joule-system/proxmox", JWT: &fakeJWT{}}
	if _, err := o.Secret(context.Background()); err == nil {
		t.Error("a login refused by OpenBao must be an error")
	}
}
