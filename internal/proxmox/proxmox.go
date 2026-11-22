// Package proxmox powers a tenant's worker VMs off and on through the Proxmox
// VE API, with an API token that can only start, stop and list VMs.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Credentials returns the API token: token_id ("user@realm!name") and token_secret.
type Credentials func(ctx context.Context) (map[string]string, error)

// Client talks to the Proxmox VE API.
type Client struct {
	// URL of the API, e.g. https://10.10.0.1:8006.
	URL         string
	Credentials Credentials
	HTTP        *http.Client
}

// NewHTTPClient trusts only the given CA (Proxmox's own pve-root-ca.pem).
func NewHTTPClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificate found")
	}

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}, nil
}

// VM is a Proxmox VM as /cluster/resources lists it.
type VM struct {
	ID     int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
	Tags   string `json:"tags"`
}

// Running reports whether the VM is running.
func (v VM) Running() bool { return v.Status == "running" }

// HasTag reports whether the VM carries tag (Proxmox separates tags with ";").
func (v VM) HasTag(tag string) bool {
	return slices.Contains(strings.FieldsFunc(v.Tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), tag)
}

// VMs lists the VMs the token may see.
func (c *Client) VMs(ctx context.Context) ([]VM, error) {
	var vms []VM
	if err := c.call(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &vms); err != nil {
		return nil, err
	}

	return vms, nil
}

// Shutdown asks the guest to shut down (ACPI or guest agent), and stops it if
// it is still running after timeout.
func (c *Client) Shutdown(ctx context.Context, vm VM, timeout time.Duration) error {
	form := url.Values{"forceStop": {"1"}, "timeout": {fmt.Sprint(int(timeout.Seconds()))}}

	return c.call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/shutdown", vm.Node, vm.ID), form, nil)
}

// Start starts the VM.
func (c *Client) Start(ctx context.Context, vm VM) error {
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", vm.Node, vm.ID), url.Values{}, nil)
}

func (c *Client) call(ctx context.Context, method, path string, form url.Values, out any) error {
	creds, err := c.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("proxmox credentials: %w", err)
	}

	if creds["token_id"] == "" || creds["token_secret"] == "" {
		return errors.New("proxmox credentials: token_id or token_secret missing")
	}

	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+"/api2/json"+path, body)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "PVEAPIToken="+creds["token_id"]+"="+creds["token_secret"])

	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("proxmox %s %s: HTTP %d %s", method, path, resp.StatusCode, resp.Status)
	}

	if out == nil {
		return nil
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}

	return json.Unmarshal(envelope.Data, out)
}

// Power switches a tenant's VMs off and on. It only touches VMs that are both
// listed in the budget and tagged "tenant-<tenant>" in Proxmox.
type Power struct {
	Client *Client
	// ShutdownTimeout before a VM that ignores the shutdown is stopped.
	ShutdownTimeout time.Duration

	mu sync.Mutex
	// shuttingDown holds when a shutdown was sent, so a VM still shutting
	// down is not sent another one (Proxmox would refuse it: VM locked).
	shuttingDown map[string]time.Time
	now          func() time.Time
}

// Off shuts down the tenant's running VMs among names, and returns the names
// of those it shut down.
func (p *Power) Off(ctx context.Context, tenant string, names []string) ([]string, error) {
	vms, err := p.tenantVMs(ctx, tenant, names)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now
	if p.now != nil {
		now = p.now
	}

	if p.shuttingDown == nil {
		p.shuttingDown = map[string]time.Time{}
	}

	var off []string

	for _, vm := range vms {
		if !vm.Running() {
			delete(p.shuttingDown, vm.Name)
			continue
		}

		// a shutdown in progress has until ShutdownTimeout (then a forced stop)
		if sent, ok := p.shuttingDown[vm.Name]; ok && now().Sub(sent) < p.ShutdownTimeout+time.Minute {
			continue
		}

		if err := p.Client.Shutdown(ctx, vm, p.ShutdownTimeout); err != nil {
			return off, fmt.Errorf("shutting down %s: %w", vm.Name, err)
		}

		p.shuttingDown[vm.Name] = now()
		off = append(off, vm.Name)
	}

	return off, nil
}

// On starts the tenant's stopped VMs among names.
func (p *Power) On(ctx context.Context, tenant string, names []string) error {
	vms, err := p.tenantVMs(ctx, tenant, names)
	if err != nil {
		return err
	}

	for _, vm := range vms {
		if vm.Running() {
			continue
		}

		if err := p.Client.Start(ctx, vm); err != nil {
			return fmt.Errorf("starting %s: %w", vm.Name, err)
		}
	}

	return nil
}

func (p *Power) tenantVMs(ctx context.Context, tenant string, names []string) ([]VM, error) {
	if len(names) == 0 {
		return nil, nil
	}

	all, err := p.Client.VMs(ctx)
	if err != nil {
		return nil, err
	}

	var vms []VM

	for _, vm := range all {
		if slices.Contains(names, vm.Name) && vm.HasTag("tenant-"+tenant) {
			vms = append(vms, vm)
		}
	}

	return vms, nil
}
