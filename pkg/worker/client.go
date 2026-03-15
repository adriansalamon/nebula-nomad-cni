package worker

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client is a client for communicating with Nebula worker processes via Unix socket.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient creates a new worker client.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    5 * time.Second,
	}
}

// sendRequest sends a request to the worker and returns the response.
func (c *Client) sendRequest(req Request) (*Response, error) {
	// Connect to worker socket
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to worker: %w", err)
	}
	defer conn.Close()

	// Set deadline for entire operation
	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("failed to set deadline: %w", err)
	}

	// Send request
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	var resp Response
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &resp, nil
}

// Ping checks if the worker is alive.
func (c *Client) Ping() error {
	resp, err := c.sendRequest(Request{Command: CommandPing})
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("ping failed: %s", resp.Error)
	}

	return nil
}

// GetConfig retrieves the current config from the worker.
func (c *Client) GetConfig() (string, error) {
	resp, err := c.sendRequest(Request{Command: CommandGetConfig})
	if err != nil {
		return "", err
	}

	if !resp.Success {
		return "", fmt.Errorf("get config failed: %s", resp.Error)
	}

	return resp.Data, nil
}

// Reload sends a config reload command to the worker.
func (c *Client) Reload(newConfigYAML string) error {
	resp, err := c.sendRequest(Request{
		Command: CommandReload,
		Data:    newConfigYAML,
	})
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("reload failed: %s", resp.Error)
	}

	return nil
}

// Stop sends a stop command to the worker.
func (c *Client) Stop() error {
	resp, err := c.sendRequest(Request{Command: CommandStop})
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("stop failed: %s", resp.Error)
	}

	return nil
}

// GetSocketPath returns the socket path for a given allocation ID.
func GetSocketPath(allocID string) string {
	return fmt.Sprintf("/run/nebula-worker-%s.sock", allocID)
}

// GetUnitName returns the systemd unit name for a given allocation ID.
func GetUnitName(allocID string) string {
	return fmt.Sprintf("nebula-nomad-worker@%s.service", allocID)
}
