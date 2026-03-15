package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/client"
	"github.com/hashicorp/consul/api"
)

const (
	// Consul KV prefixes
	consulIPPoolKey      = "nebula-cni/ip-pool"
	consulAllocationsKey = "nebula-cni/allocations"
)

// ConsulManager handles IP allocation and state management via Consul KV store.
type ConsulManager struct {
	client *api.Client
}

// IPPool represents the pool of available IP addresses.
type IPPool struct {
	NetworkCIDR string   `json:"network_cidr"` // Full network CIDR (e.g., "10.64.32.0/19")
	RangeStart  string   `json:"range_start"`  // First IP in pool (e.g., "10.64.63.1")
	RangeEnd    string   `json:"range_end"`    // Last IP in pool (e.g., "10.64.63.254")
	Allocated   []string `json:"allocated"`    // List of allocated IPs
}

// NewConsulManager creates a new Consul manager for IP allocation.
func NewConsulManager(consulAddr string) (*ConsulManager, error) {
	config := api.DefaultConfig()
	if consulAddr != "" {
		config.Address = consulAddr
	}

	client, err := api.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consul client: %w", err)
	}

	return &ConsulManager{
		client: client,
	}, nil
}

// InitializeIPPool initializes the IP pool in Consul if it doesn't exist.
func (cm *ConsulManager) InitializeIPPool(networkCIDR, rangeStart, rangeEnd string) error {
	kv := cm.client.KV()

	// Check if pool already exists
	pair, _, err := kv.Get(consulIPPoolKey, nil)
	if err != nil {
		return fmt.Errorf("failed to check existing pool: %w", err)
	}

	if pair != nil {
		return nil // Pool already exists, nothing to do
	}

	// Validate network CIDR
	_, ipNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("invalid network CIDR: %w", err)
	}

	// Validate range IPs
	startIP := net.ParseIP(rangeStart)
	if startIP == nil {
		return fmt.Errorf("invalid range start IP: %s", rangeStart)
	}
	endIP := net.ParseIP(rangeEnd)
	if endIP == nil {
		return fmt.Errorf("invalid range end IP: %s", rangeEnd)
	}

	// Verify range is within network
	if !ipNet.Contains(startIP) {
		return fmt.Errorf("range start %s is not within network %s", rangeStart, networkCIDR)
	}
	if !ipNet.Contains(endIP) {
		return fmt.Errorf("range end %s is not within network %s", rangeEnd, networkCIDR)
	}

	// Verify start <= end
	if ipToInt(startIP) > ipToInt(endIP) {
		return fmt.Errorf("range start %s must be <= range end %s", rangeStart, rangeEnd)
	}

	// Create new pool
	pool := &IPPool{
		NetworkCIDR: networkCIDR,
		RangeStart:  rangeStart,
		RangeEnd:    rangeEnd,
		Allocated:   []string{},
	}

	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("failed to marshal pool: %w", err)
	}

	// Write to Consul
	p := &api.KVPair{
		Key:   consulIPPoolKey,
		Value: data,
	}

	_, err = kv.Put(p, nil)
	if err != nil {
		return fmt.Errorf("failed to write pool to consul: %w", err)
	}

	return nil
}

// AllocateIP atomically allocates an IP address from the pool.
func (cm *ConsulManager) AllocateIP() (string, error) {
	kv := cm.client.KV()
	maxRetries := 10

	for i := range maxRetries {
		// Get current pool
		pair, _, err := kv.Get(consulIPPoolKey, nil)
		if err != nil {
			return "", fmt.Errorf("failed to get IP pool: %w", err)
		}

		if pair == nil {
			return "", fmt.Errorf("IP pool not initialized")
		}

		var pool IPPool
		if err := json.Unmarshal(pair.Value, &pool); err != nil {
			return "", fmt.Errorf("failed to unmarshal pool: %w", err)
		}

		// Find next available IP
		ip, err := cm.findNextAvailableIP(&pool)
		if err != nil {
			return "", err
		}

		// Add to allocated list
		pool.Allocated = append(pool.Allocated, ip)
		sort.Strings(pool.Allocated) // Keep sorted for consistency

		// Marshal updated pool
		data, err := json.Marshal(pool)
		if err != nil {
			return "", fmt.Errorf("failed to marshal updated pool: %w", err)
		}

		// Attempt atomic CAS (Check-And-Set)
		p := &api.KVPair{
			Key:         consulIPPoolKey,
			Value:       data,
			ModifyIndex: pair.ModifyIndex,
		}

		success, _, err := kv.CAS(p, nil)
		if err != nil {
			return "", fmt.Errorf("failed to update pool: %w", err)
		}

		if success {
			return ip, nil
		}

		// CAS failed, retry
		time.Sleep(time.Millisecond * 100 * time.Duration(i+1))
	}

	return "", fmt.Errorf("failed to allocate IP after %d retries (too much contention)", maxRetries)
}

// ReleaseIP releases an IP address back to the pool.
// Accepts IP with or without CIDR notation (e.g., "10.99.0.1/24" or "10.99.0.1")
func (cm *ConsulManager) ReleaseIP(ip string) error {
	kv := cm.client.KV()
	maxRetries := 10

	// Normalize the IP - compare both with and without CIDR notation
	ipOnly := ip
	if before, _, ok := strings.Cut(ip, "/"); ok {
		ipOnly = before
	}

	for i := range maxRetries {
		// Get current pool
		pair, _, err := kv.Get(consulIPPoolKey, nil)
		if err != nil {
			return fmt.Errorf("failed to get IP pool: %w", err)
		}

		if pair == nil {
			return fmt.Errorf("IP pool not initialized")
		}

		var pool IPPool
		if err := json.Unmarshal(pair.Value, &pool); err != nil {
			return fmt.Errorf("failed to unmarshal pool: %w", err)
		}

		// Remove from allocated list
		// Match both "10.99.0.1" and "10.99.0.1/24" formats
		found := false
		newAllocated := make([]string, 0, len(pool.Allocated))
		for _, allocIP := range pool.Allocated {
			allocIPOnly := allocIP
			if before, _, ok := strings.Cut(allocIP, "/"); ok {
				allocIPOnly = before
			}

			if allocIPOnly == ipOnly {
				found = true
				continue
			}
			newAllocated = append(newAllocated, allocIP)
		}

		if !found {
			// IP wasn't allocated, nothing to do
			return nil
		}

		pool.Allocated = newAllocated

		// Marshal updated pool
		data, err := json.Marshal(pool)
		if err != nil {
			return fmt.Errorf("failed to marshal updated pool: %w", err)
		}

		// Attempt atomic CAS
		p := &api.KVPair{
			Key:         consulIPPoolKey,
			Value:       data,
			ModifyIndex: pair.ModifyIndex,
		}

		success, _, err := kv.CAS(p, nil)
		if err != nil {
			return fmt.Errorf("failed to update pool: %w", err)
		}

		if success {
			return nil
		}

		// CAS failed, retry
		time.Sleep(time.Millisecond * 100 * time.Duration(i+1))
	}

	return fmt.Errorf("failed to release IP after %d retries", maxRetries)
}

// StoreAllocationRecord stores an allocation record in Consul.
func (cm *ConsulManager) StoreAllocationRecord(record *client.AllocationRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal allocation record: %w", err)
	}

	key := fmt.Sprintf("%s/%s", consulAllocationsKey, record.AllocID)
	p := &api.KVPair{
		Key:   key,
		Value: data,
	}

	_, err = cm.client.KV().Put(p, nil)
	if err != nil {
		return fmt.Errorf("failed to store allocation record: %w", err)
	}

	return nil
}

// GetAllocationRecord retrieves an allocation record from Consul.
func (cm *ConsulManager) GetAllocationRecord(allocID string) (*client.AllocationRecord, error) {
	key := fmt.Sprintf("%s/%s", consulAllocationsKey, allocID)
	pair, _, err := cm.client.KV().Get(key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocation record: %w", err)
	}

	if pair == nil {
		return nil, nil
	}

	var record client.AllocationRecord
	if err := json.Unmarshal(pair.Value, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal allocation record: %w", err)
	}

	return &record, nil
}

// DeleteAllocationRecord deletes an allocation record from Consul.
func (cm *ConsulManager) DeleteAllocationRecord(allocID string) error {
	key := fmt.Sprintf("%s/%s", consulAllocationsKey, allocID)
	_, err := cm.client.KV().Delete(key, nil)
	if err != nil {
		return fmt.Errorf("failed to delete allocation record: %w", err)
	}

	return nil
}

// GetAllAllocations retrieves all allocation records from Consul.
func (cm *ConsulManager) GetAllAllocations() ([]*client.AllocationRecord, error) {
	pairs, _, err := cm.client.KV().List(consulAllocationsKey+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list allocations: %w", err)
	}

	if len(pairs) == 0 {
		return nil, nil
	}

	records := make([]*client.AllocationRecord, 0, len(pairs))
	for _, pair := range pairs {
		var record client.AllocationRecord
		if err := json.Unmarshal(pair.Value, &record); err != nil {
			// Skip invalid records
			continue
		}
		records = append(records, &record)
	}

	return records, nil
}

// findNextAvailableIP finds the next available IP in the range that's not allocated.
// Returns IP with network CIDR notation (e.g., "10.64.63.5/19")
func (cm *ConsulManager) findNextAvailableIP(pool *IPPool) (string, error) {
	// Parse network CIDR to get subnet mask
	_, ipNet, err := net.ParseCIDR(pool.NetworkCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid network CIDR in pool: %w", err)
	}

	// Parse range boundaries
	startIP := net.ParseIP(pool.RangeStart)
	if startIP == nil {
		return "", fmt.Errorf("invalid range start IP in pool: %s", pool.RangeStart)
	}
	endIP := net.ParseIP(pool.RangeEnd)
	if endIP == nil {
		return "", fmt.Errorf("invalid range end IP in pool: %s", pool.RangeEnd)
	}

	// Get the subnet mask size (e.g., 19 for /19)
	maskSize, _ := ipNet.Mask.Size()

	// Create a map of allocated IPs for fast lookup
	allocated := make(map[string]bool)
	for _, allocIP := range pool.Allocated {
		// Remove /XX suffix if present
		ipOnly := allocIP
		if before, _, ok := strings.Cut(allocIP, "/"); ok {
			ipOnly = before
		}
		allocated[ipOnly] = true
	}

	// Iterate through IPs in the range
	startInt := ipToInt(startIP)
	endInt := ipToInt(endIP)

	for ipInt := startInt; ipInt <= endInt; ipInt++ {
		ip := intToIP(ipInt)
		ipStr := ip.String()

		// Check if IP is available
		if !allocated[ipStr] {
			// Return IP with network CIDR notation
			return fmt.Sprintf("%s/%d", ipStr, maskSize), nil
		}
	}

	return "", fmt.Errorf("no available IPs in range %s-%s", pool.RangeStart, pool.RangeEnd)
}

// inc increments an IP address.
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// ipToInt converts an IP address to a uint32 (for IPv4).
func ipToInt(ip net.IP) uint32 {
	// Ensure we're working with IPv4
	ip = ip.To4()
	if ip == nil {
		return 0
	}

	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// intToIP converts a uint32 to an IP address.
func intToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
