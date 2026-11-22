package proxmox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePVE is a Proxmox API with a few VMs; it records the power calls.
type fakePVE struct {
	mu    sync.Mutex
	vms   []VM
	calls []string
	auth  []string
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.auth = append(f.auth, r.Header.Get("Authorization"))

	if r.URL.Path == "/api2/json/cluster/resources" {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": f.vms})
		return
	}

	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/") {
		_ = r.ParseForm()
		f.calls = append(f.calls, strings.TrimPrefix(r.URL.Path, "/api2/json")+"?"+r.PostForm.Encode())
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve:task"})

		return
	}

	http.NotFound(w, r)
}

func newPower(t *testing.T, f *fakePVE) *Power {
	t.Helper()

	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)

	creds := func(context.Context) (map[string]string, error) {
		return map[string]string{"token_id": "joule-budget@pve!controller", "token_secret": "s3cret"}, nil
	}

	return &Power{
		Client:          &Client{URL: srv.URL, Credentials: creds, HTTP: srv.Client()},
		ShutdownTimeout: 3 * time.Minute,
	}
}

func cluster() *fakePVE {
	return &fakePVE{vms: []VM{
		{ID: 141, Name: "atlas-worker-141", Node: "pve", Status: "running", Tags: "joule;tenant-atlas"},
		{ID: 142, Name: "atlas-worker-142", Node: "pve", Status: "stopped", Tags: "joule;tenant-atlas"},
		{ID: 150, Name: "orion-worker-150", Node: "pve", Status: "running", Tags: "joule;tenant-orion"},
		{ID: 100, Name: "hub", Node: "pve", Status: "running", Tags: ""},
	}}
}

func TestOffShutsDownOnlyTheTenantsRunningVMs(t *testing.T) {
	f := cluster()

	// hub and orion's VM are listed too: they must not be touched, as they
	// do not carry atlas's tag
	off, err := newPower(t, f).Off(context.Background(), "atlas",
		[]string{"atlas-worker-141", "atlas-worker-142", "orion-worker-150", "hub"})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(off, []string{"atlas-worker-141"}) {
		t.Errorf("shut down %v, want only atlas-worker-141", off)
	}

	if want := []string{"/nodes/pve/qemu/141/status/shutdown?forceStop=1&timeout=180"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}

	if f.auth[0] != "PVEAPIToken=joule-budget@pve!controller=s3cret" {
		t.Errorf("authorization header = %q", f.auth[0])
	}
}

func TestOffDoesNotRepeatAShutdownInProgress(t *testing.T) {
	f := cluster()
	p := newPower(t, f)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	for range 3 { // the VM keeps reporting "running" while it shuts down
		if _, err := p.Off(context.Background(), "atlas", []string{"atlas-worker-141"}); err != nil {
			t.Fatal(err)
		}

		now = now.Add(time.Minute)
	}

	if len(f.calls) != 1 {
		t.Fatalf("shutdowns sent = %d, want 1: %v", len(f.calls), f.calls)
	}

	// still running long after the forced-stop deadline: try again
	now = now.Add(5 * time.Minute)
	if _, err := p.Off(context.Background(), "atlas", []string{"atlas-worker-141"}); err != nil {
		t.Fatal(err)
	}

	if len(f.calls) != 2 {
		t.Errorf("shutdowns sent = %d, want 2 after the deadline", len(f.calls))
	}
}

func TestOnStartsOnlyTheTenantsStoppedVMs(t *testing.T) {
	f := cluster()

	if err := newPower(t, f).On(context.Background(), "atlas", []string{"atlas-worker-141", "atlas-worker-142", "orion-worker-150"}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/nodes/pve/qemu/142/status/start?"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestNoVMsNoCall(t *testing.T) {
	f := cluster()

	off, err := newPower(t, f).Off(context.Background(), "atlas", nil)
	if err != nil || len(off) != 0 || len(f.auth) != 0 {
		t.Errorf("off=%v err=%v requests=%d", off, err, len(f.auth))
	}
}

func TestCredentialErrorsAreReturned(t *testing.T) {
	p := newPower(t, cluster())
	p.Client.Credentials = func(context.Context) (map[string]string, error) { return nil, errors.New("openbao down") }

	if _, err := p.Off(context.Background(), "atlas", []string{"atlas-worker-141"}); err == nil || !strings.Contains(err.Error(), "openbao down") {
		t.Errorf("err = %v", err)
	}
}

func TestUntrustedCertificateIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(cluster())
	t.Cleanup(srv.Close)

	// trust only an unrelated CA, as if Proxmox's certificate were not ours
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	httpClient, err := NewHTTPClient(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}

	c := &Client{URL: srv.URL, HTTP: httpClient, Credentials: func(context.Context) (map[string]string, error) {
		return map[string]string{"token_id": "a", "token_secret": "b"}, nil
	}}
	if _, err := c.VMs(context.Background()); err == nil {
		t.Error("a server with an untrusted certificate was accepted")
	}

	if _, err := NewHTTPClient([]byte("no certificate")); err == nil {
		t.Error("a CA file without a certificate was accepted")
	}
}

func TestHasTag(t *testing.T) {
	vm := VM{Tags: "joule;tenant-atlas"}
	if !vm.HasTag("tenant-atlas") || vm.HasTag("tenant-atla") || vm.HasTag("atlas") {
		t.Error("tag matching must be exact")
	}
}
