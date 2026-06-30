package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kkjorsvik/smith/internal/types"
)

// Client posts GitOps resources to the smith control-plane API using bearer
// token authentication.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client from cfg, trimming any trailing slash from the server URL.
func New(cfg Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.Server, "/"),
		token:   cfg.Token,
		// A bounded timeout so the CLI can't hang forever on an unresponsive
		// control plane (no per-request context is threaded for a short-lived CLI).
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// ApplyWorkload creates or updates a workload via POST /workloads.
func (c *Client) ApplyWorkload(w types.Workload) error {
	return c.post("/workloads", w)
}

// ApplyService creates or updates a service via POST /services.
func (c *Client) ApplyService(s types.Service) error {
	return c.post("/services", s)
}

// ApplyIngress creates or updates an ingress via POST /ingresses.
func (c *Client) ApplyIngress(i types.Ingress) error {
	return c.post("/ingresses", i)
}

// post marshals v as JSON and POSTs it to baseURL+path with the bearer token.
// A 2xx status is success; otherwise the response body is folded into the error.
func (c *Client) post(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: marshal: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return respError(path, resp)
}

// respError reads a non-2xx response body and folds the status into an error.
func respError(path string, resp *http.Response) error {
	respBody, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(respBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (check token): %s", msg)
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
}

// get issues an authenticated GET and decodes a 2xx JSON body into out.
func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respError(path, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
}

// postInto issues an authenticated POST with no request body and decodes a 2xx
// JSON response into out.
func (c *Client) postInto(path string, out any) error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respError(path, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
}

// del issues an authenticated DELETE; any 2xx is success.
func (c *Client) del(path string) error {
	req, err := http.NewRequest(http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return respError(path, resp)
}

// ListWorkloads returns all workloads via GET /workloads.
func (c *Client) ListWorkloads() ([]types.Workload, error) {
	var out []types.Workload
	if err := c.get("/workloads", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListServices returns all services via GET /services.
func (c *Client) ListServices() ([]types.Service, error) {
	var out []types.Service
	if err := c.get("/services", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListIngresses returns all ingresses via GET /ingresses.
func (c *Client) ListIngresses() ([]types.Ingress, error) {
	var out []types.Ingress
	if err := c.get("/ingresses", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RebalancePlan returns the moves a rebalance would make via GET /rebalance,
// without enacting them.
func (c *Client) RebalancePlan() ([]types.Move, error) {
	var out []types.Move
	if err := c.get("/rebalance", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Rebalance enacts a rebalance via POST /rebalance and returns the moves
// committed.
func (c *Client) Rebalance() ([]types.Move, error) {
	var out []types.Move
	if err := c.postInto("/rebalance", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteWorkload removes a workload via DELETE /workloads/{id}.
func (c *Client) DeleteWorkload(id string) error {
	return c.del("/workloads/" + url.PathEscape(id))
}

// DeleteService removes a service via DELETE /services/{name}.
func (c *Client) DeleteService(name string) error {
	return c.del("/services/" + url.PathEscape(name))
}

// DeleteIngress removes an ingress via DELETE /ingresses/{host}.
func (c *Client) DeleteIngress(host string) error {
	return c.del("/ingresses/" + url.PathEscape(host))
}
