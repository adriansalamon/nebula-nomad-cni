package agent

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/hashicorp/nomad/api"
)

// NomadClient wraps the Nomad API client for querying allocation metadata.
type NomadClient struct {
	client *api.Client
}

// TaskMetadata contains all the metadata extracted from a Nomad task.
type TaskMetadata struct {
	JobID        string
	TaskGroup    string
	TaskName     string
	Roles        []string
	NebulaConfig string
	AllMeta      map[string]string
	NodeID       string // Nomad node ID where allocation is running
	NodeName     string // Nomad node name where allocation is running
}

// NewNomadClient creates a new Nomad API client.
func NewNomadClient(nomadAddr string) (*NomadClient, error) {
	config := api.DefaultConfig()
	if nomadAddr != "" {
		config.Address = nomadAddr
	}

	client, err := api.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Nomad client: %w", err)
	}

	return &NomadClient{
		client: client,
	}, nil
}

// GetTaskMetadata retrieves all task metadata for a given allocation ID.
func (nc *NomadClient) GetTaskMetadata(allocID string) (*TaskMetadata, error) {
	alloc, _, err := nc.client.Allocations().Info(allocID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocation info: %w", err)
	}

	if alloc == nil {
		return nil, fmt.Errorf("allocation not found: %s", allocID)
	}

	job := alloc.Job
	if job == nil {
		return nil, fmt.Errorf("allocation has no associated job")
	}

	// Find the task group
	var taskGroup *api.TaskGroup
	for _, tg := range job.TaskGroups {
		if *tg.Name == alloc.TaskGroup {
			taskGroup = tg
			break
		}
	}

	if taskGroup == nil {
		return nil, fmt.Errorf("task group not found: %s", alloc.TaskGroup)
	}

	// For CNI, we typically have one task per group, but let's find the first task
	// that has network configuration or just use the first task
	var selectedTask *api.Task
	for _, task := range taskGroup.Tasks {
		selectedTask = task
		break
	}

	if selectedTask == nil {
		return nil, fmt.Errorf("no tasks found in task group")
	}

	// Collect metadata (job-level, then task-level override)
	allMeta := make(map[string]string)

	if job.Meta != nil {
		maps.Copy(allMeta, job.Meta)
	}

	if selectedTask.Meta != nil {
		maps.Copy(allMeta, selectedTask.Meta)
	}

	// Extract roles from metadata
	var roles []string
	if rolesStr, ok := allMeta["nebula_roles"]; ok {
		// Parse comma-separated roles
		err := json.Unmarshal([]byte(rolesStr), &roles)
		if err != nil {
			return nil, fmt.Errorf("failed to parse roles: %w", err)
		}
	}

	// Extract Nebula config override
	nebulaConfig := allMeta["nebula_config"]

	metadata := &TaskMetadata{
		JobID:        *job.ID,
		TaskGroup:    alloc.TaskGroup,
		TaskName:     selectedTask.Name,
		Roles:        roles,
		NebulaConfig: nebulaConfig,
		AllMeta:      allMeta,
		NodeID:       alloc.NodeID,
		NodeName:     alloc.NodeName,
	}

	return metadata, nil
}

// GetLocalNodeID returns the node ID of the local Nomad client.
func (nc *NomadClient) GetLocalNodeID() (string, error) {
	nodeID := ""
	nodes, _, err := nc.client.Nodes().List(nil)
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	// Try to find local node by comparing with local hostname
	// This is a heuristic - Nomad API doesn't have a direct "get local node" call
	for _, node := range nodes {
		if node.Status == "ready" {
			// Return first ready node - in single-node setup this works
			// In multi-node, caller should pass node ID explicitly
			nodeID = node.ID
			break
		}
	}

	if nodeID == "" {
		return "", fmt.Errorf("no ready nodes found")
	}

	return nodeID, nil
}

// GetNodeAllocations returns all running allocation IDs for a given node.
func (nc *NomadClient) GetNodeAllocations(nodeID string) ([]string, error) {
	allocs, _, err := nc.client.Nodes().Allocations(nodeID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get node allocations: %w", err)
	}

	allocIDs := make([]string, 0)
	for _, alloc := range allocs {
		// Only include running allocations
		if alloc.ClientStatus == "running" {
			allocIDs = append(allocIDs, alloc.ID)
		}
	}

	return allocIDs, nil
}

// splitAndTrim splits a string by delimiter and trims whitespace.
func splitAndTrim(s, delim string) []string {
	if s == "" {
		return nil
	}

	parts := make([]string, 0)
	for _, part := range splitString(s, delim) {
		trimmed := trimSpace(part)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

// splitString is a simple string split helper.
func splitString(s, delim string) []string {
	if s == "" {
		return nil
	}

	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if i+len(delim) <= len(s) && s[i:i+len(delim)] == delim {
			result = append(result, s[start:i])
			start = i + len(delim)
			i += len(delim) - 1
		}
	}
	result = append(result, s[start:])
	return result
}

// trimSpace removes leading and trailing whitespace.
func trimSpace(s string) string {
	start := 0
	end := len(s)

	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}

	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}

	return s[start:end]
}
