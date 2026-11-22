// Package credentials fetches the controller's secrets from OpenBao, logging in
// with the controller's SPIFFE identity: no secret is stored in Kubernetes.
package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// JWTSource returns a JWT-SVID for an audience.
type JWTSource interface {
	FetchJWT(ctx context.Context, audience string) (string, error)
}

// SPIFFE fetches JWT-SVIDs from the SPIRE agent's Workload API.
type SPIFFE struct {
	// Socket is the Workload API address, e.g. unix:///spiffe-workload-api/spire-agent.sock.
	Socket string
}

// FetchJWT implements JWTSource.
func (s SPIFFE) FetchJWT(ctx context.Context, audience string) (string, error) {
	svid, err := workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience}, workloadapi.WithAddr(s.Socket))
	if err != nil {
		return "", fmt.Errorf("fetching JWT-SVID: %w", err)
	}

	return svid.Marshal(), nil
}

// OpenBao reads a KV v2 secret after a JWT login with a SPIFFE JWT-SVID.
// The secret is cached for TTL, then read again with a fresh login.
type OpenBao struct {
	// Address of OpenBao, e.g. http://openbao.openbao.svc:8200.
	Address string
	// Role of the jwt auth method; its bound_subject is our SPIFFE ID.
	Role string
	// Audience of the JWT-SVID, the role's bound_audiences.
	Audience string
	// Path of the secret under the KV v2 mount "secret", e.g. joule-system/proxmox.
	Path string
	// TTL of the cached secret.
	TTL time.Duration

	JWT  JWTSource
	HTTP *http.Client

	mu      sync.Mutex
	cached  map[string]string
	expires time.Time
	now     func() time.Time
}

// Secret returns the secret's key/value data.
func (o *OpenBao) Secret(ctx context.Context) (map[string]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := time.Now
	if o.now != nil {
		now = o.now
	}

	if o.cached != nil && now().Before(o.expires) {
		return o.cached, nil
	}

	jwt, err := o.JWT.FetchJWT(ctx, o.Audience)
	if err != nil {
		return nil, err
	}

	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := o.do(ctx, http.MethodPost, "/v1/auth/jwt/login", "", map[string]string{"role": o.Role, "jwt": jwt}, &login); err != nil {
		return nil, fmt.Errorf("openbao login as %s: %w", o.Role, err)
	}

	token := login.Auth.ClientToken
	// The login token is only needed for this read: revoke it right away.
	defer func() {
		_ = o.do(context.WithoutCancel(ctx), http.MethodPost, "/v1/auth/token/revoke-self", token, nil, nil)
	}()

	var secret struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodGet, "/v1/secret/data/"+strings.TrimPrefix(o.Path, "/"), token, nil, &secret); err != nil {
		return nil, fmt.Errorf("reading secret/%s: %w", o.Path, err)
	}

	o.cached = secret.Data.Data
	o.expires = now().Add(o.TTL)

	return o.cached, nil
}

func (o *OpenBao) do(ctx context.Context, method, path, token string, body, out any) error {
	var reader *bytes.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(o.Address, "/")+path, reader)
	if err != nil {
		return err
	}

	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}

	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}

	if out == nil {
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}
