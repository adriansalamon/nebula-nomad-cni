package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// DefaultSocketPath is the default unix socket path for the agent
	DefaultSocketPath = "/var/run/nebula-cni.sock"

	// DefaultTimeout is the default timeout for agent requests
	DefaultTimeout = 30 * time.Second
)

// Client is a client for communicating with the nebula-nomad-agent
// via a unix domain socket.
type Client struct {
	socketPath string
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new client for the agent at the specified socket path.
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}

	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
		timeout: DefaultTimeout,
	}
}

// SetTimeout sets the timeout for requests to the agent.
func (c *Client) SetTimeout(timeout time.Duration) {
	c.timeout = timeout
}

// Allocate requests the agent to allocate a Nebula IP and sign a certificate
// for the given Nomad task.
func (c *Client) Allocate(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	// Marshal request to JSON
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request with timeout
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://unix/allocate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Parse response
	var allocResp AllocateResponse
	if err := json.NewDecoder(resp.Body).Decode(&allocResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		if allocResp.Error != "" {
			return &allocResp, fmt.Errorf("agent returned error (status %d): %s", resp.StatusCode, allocResp.Error)
		}
		return &allocResp, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	return &allocResp, nil
}

// Deallocate requests the agent to release the Nebula resources for the
// given allocation ID.
func (c *Client) Deallocate(ctx context.Context, allocID string) (*DeallocateResponse, error) {
	// Create HTTP request with timeout
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	url := fmt.Sprintf("http://unix/allocate/%s", allocID)
	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Send request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Parse response
	var deallocResp DeallocateResponse
	if err := json.NewDecoder(resp.Body).Decode(&deallocResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		if deallocResp.Error != "" {
			return &deallocResp, fmt.Errorf("agent returned error (status %d): %s", resp.StatusCode, deallocResp.Error)
		}
		return &deallocResp, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	return &deallocResp, nil
}

// Ping checks if the agent is reachable and responding.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to ping agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	return nil
}
