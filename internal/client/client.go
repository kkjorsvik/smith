package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
		http:    http.DefaultClient,
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
	respBody, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(respBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized (check token): %s", msg)
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
}
