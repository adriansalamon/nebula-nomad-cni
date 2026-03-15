package client

// AllocateRequest represents the request to allocate a Nebula IP and certificate
// for a Nomad task. The agent will query the Nomad API to get job metadata.
type AllocateRequest struct {
	// AllocID is the Nomad allocation ID
	AllocID string `json:"alloc_id"`

	// NetNS is the network namespace path for this task
	NetNS string `json:"netns"`
}

// AllocateResponse represents the response from the agent after allocating
// an IP and signing a certificate.
type AllocateResponse struct {
	// Success indicates whether the operation succeeded
	Success bool `json:"success"`

	// IP is the allocated Nebula IP address
	IP string `json:"ip,omitempty"`

	// Cert is the signed Nebula certificate (PEM encoded)
	Cert string `json:"cert,omitempty"`

	// CertKey is the private key for the certificate (PEM encoded)
	CertKey string `json:"cert_key,omitempty"`

	// Error message if Success is false
	Error string `json:"error,omitempty"`
}

// DeallocateResponse represents the response from deallocating a task's
// Nebula resources.
type DeallocateResponse struct {
	// Success indicates whether the operation succeeded
	Success bool `json:"success"`

	// Error message if Success is false
	Error string `json:"error,omitempty"`
}

// AllocationRecord represents the minimal state stored in Consul for each allocation.
// Nomad API is the source of truth for job metadata - we only store what's needed for IP management.
type AllocationRecord struct {
	// AllocID is the Nomad allocation ID (primary key)
	AllocID string `json:"alloc_id"`

	// IP is the allocated Nebula IP (for IP pool management)
	IP string `json:"ip"`

	// NodeID is the Nomad node ID running this allocation (for filtering local allocations)
	NodeID string `json:"node_id"`

	// NetNS is the network namespace path for this task
	NetNS string `json:"netns"`
}
