package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/client"
	"github.com/gorilla/mux"
)

// Config holds the configuration for the agent.
type Config struct {
	SocketPath   string
	ConsulAddr   string
	NomadAddr    string
	CACertPath   string
	CAKeyPath    string
	NebulaConfig string
	CertTTL      time.Duration
}

// Agent is the main agent service that handles IP allocation and certificate signing.
type Agent struct {
	config        *Config
	consulManager *ConsulManager
	nomadClient   *NomadClient
	certSigner    *CertificateSigner
	nebulaManager *NebulaManager
	httpServer    *http.Server
	listener      net.Listener
}

// NewAgent creates a new agent instance.
func NewAgent(config *Config) (*Agent, error) {
	// Create Consul manager
	consulManager, err := NewConsulManager(config.ConsulAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create Consul manager: %w", err)
	}

	nomadClient, err := NewNomadClient(config.NomadAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create Nomad client: %w", err)
	}

	certSigner, err := NewCertificateSigner(config.CACertPath, config.CAKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate signer: %w", err)
	}

	nebulaManager := NewNebulaManager(config.NebulaConfig)

	return &Agent{
		config:        config,
		consulManager: consulManager,
		nomadClient:   nomadClient,
		certSigner:    certSigner,
		nebulaManager: nebulaManager,
	}, nil
}

// Start starts the agent HTTP server on the unix socket.
func (a *Agent) Start() error {
	// Clean up stale allocations on startup
	if err := a.cleanupStaleAllocations(); err != nil {
		log.Printf("Warning: failed to cleanup stale allocations: %v", err)
		// Don't fail startup, just log warning
	}

	// Restart active Nebula instances for allocations on this node
	if err := a.restartLocalInstances(); err != nil {
		log.Printf("Warning: failed to restart local instances: %v", err)
		// Don't fail startup, just log warning
	}

	// Remove existing socket if it exists
	if err := os.RemoveAll(a.config.SocketPath); err != nil {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create unix socket listener
	listener, err := net.Listen("unix", a.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to create unix socket: %w", err)
	}

	// Set socket permissions
	if err := os.Chmod(a.config.SocketPath, 0777); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	a.listener = listener

	// Create HTTP router
	router := mux.NewRouter()
	router.HandleFunc("/allocate", a.handleAllocate).Methods("POST")
	router.HandleFunc("/allocate/{alloc_id}", a.handleDeallocate).Methods("DELETE")
	router.HandleFunc("/health", a.handleHealth).Methods("GET")

	// Create HTTP server
	a.httpServer = &http.Server{
		Handler: router,
	}

	log.Printf("Agent listening on %s", a.config.SocketPath)

	// Start certificate rotation worker
	a.startCertRotationWorker()

	// Start serving
	if err := a.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}

	return nil
}

// Stop gracefully stops the agent and all running Nebula instances.
func (a *Agent) Stop() error {
	log.Printf("Stopping agent...")

	// Stop all Nebula instances first
	a.nebulaManager.StopAll()

	// Then stop HTTP server
	if a.httpServer != nil {
		return a.httpServer.Close()
	}
	return nil
}

// InitializeIPPool initializes the IP pool in Consul with a network CIDR and IP range.
func (a *Agent) InitializeIPPool(networkCIDR, rangeStart, rangeEnd string) error {
	return a.consulManager.InitializeIPPool(networkCIDR, rangeStart, rangeEnd)
}

// handleAllocate handles IP allocation and certificate signing requests.
func (a *Agent) handleAllocate(w http.ResponseWriter, r *http.Request) {
	var req client.AllocateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.sendError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}

	log.Printf("Allocate request for alloc_id=%s", req.AllocID)

	// Validate request
	if req.AllocID == "" || req.NetNS == "" {
		a.sendError(w, http.StatusBadRequest, "alloc_id and netns are required")
		return
	}

	// Query Nomad API for task metadata
	metadata, err := a.nomadClient.GetTaskMetadata(req.AllocID)
	if err != nil {
		log.Printf("Failed to get task metadata from Nomad: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get task metadata: %v", err))
		return
	}

	log.Printf("Task metadata: job=%s group=%s task=%s roles=%v", metadata.JobID, metadata.TaskGroup, metadata.TaskName, metadata.Roles)

	// Check if allocation already exists
	existing, err := a.consulManager.GetAllocationRecord(req.AllocID)
	if err != nil {
		log.Printf("Error checking existing allocation: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to check existing allocation: %v", err))
		return
	}

	if existing != nil {
		log.Printf("Allocation %s already exists with IP %s", req.AllocID, existing.IP)
		a.sendError(w, http.StatusConflict, "allocation already exists")
		return
	}

	// Allocate IP from Consul (already includes correct subnet mask from pool's NetworkCIDR)
	ip, err := a.consulManager.AllocateIP()
	if err != nil {
		log.Printf("Failed to allocate IP: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to allocate IP: %v", err))
		return
	}

	log.Printf("Allocated IP %s for alloc_id=%s", ip, req.AllocID)

	// Get CA cert for inlining
	caCertPEM, err := a.certSigner.GetCACertificate()
	if err != nil {
		log.Printf("Failed to get CA certificate: %v", err)
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get CA certificate: %v", err))
		return
	}

	// Sign certificate
	certName := fmt.Sprintf("%s.%s.%s", metadata.TaskName, metadata.TaskGroup, metadata.JobID)
	certPEM, keyPEM, err := a.certSigner.SignCertificate(ip, metadata.Roles, certName, a.config.CertTTL)
	if err != nil {
		log.Printf("Failed to sign certificate: %v", err)
		// Release IP since we failed
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to sign certificate: %v", err))
		return
	}

	log.Printf("Signed certificate for IP %s", ip)

	// Start Nebula instance with job-specific config
	if err := a.nebulaManager.StartInstance(req.AllocID, ip, certPEM, keyPEM, caCertPEM, metadata.NebulaConfig, req.NetNS); err != nil {
		log.Printf("Failed to start Nebula instance: %v", err)
		// Release IP and clean up
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to start Nebula instance: %v", err))
		return
	}

	log.Printf("Started Nebula instance for alloc_id=%s", req.AllocID)

	// Store allocation record in Consul (minimal state - Nomad API is source of truth)
	record := &client.AllocationRecord{
		AllocID: req.AllocID,
		IP:      ip,
		NodeID:  metadata.NodeID,
		NetNS:   req.NetNS,
	}

	if err := a.consulManager.StoreAllocationRecord(record); err != nil {
		log.Printf("Failed to store allocation record: %v", err)
		// Clean up Nebula instance and IP
		_ = a.nebulaManager.StopInstance(req.AllocID)
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to store allocation record: %v", err))
		return
	}

	log.Printf("Stored allocation record for alloc_id=%s", req.AllocID)

	// Send success response
	resp := &client.AllocateResponse{
		Success: true,
		IP:      ip,
		Cert:    certPEM,
		CertKey: keyPEM,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDeallocate handles deallocation requests.
func (a *Agent) handleDeallocate(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	allocID := vars["alloc_id"]

	log.Printf("Deallocate request for alloc_id=%s", allocID)

	if allocID == "" {
		a.sendError(w, http.StatusBadRequest, "alloc_id is required")
		return
	}

	// Get allocation record to find IP
	record, err := a.consulManager.GetAllocationRecord(allocID)
	if err != nil {
		log.Printf("Error getting allocation record: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get allocation record: %v", err))
		return
	}

	if record == nil {
		log.Printf("Allocation %s not found", allocID)
		// Not found, but that's okay - consider it success
		resp := &client.DeallocateResponse{Success: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Stop Nebula instance
	if err := a.nebulaManager.StopInstance(allocID); err != nil {
		log.Printf("Failed to stop Nebula instance: %v", err)
		// Continue with cleanup even if this fails
	}

	log.Printf("Stopped Nebula instance for alloc_id=%s", allocID)

	// Release IP
	if err := a.consulManager.ReleaseIP(record.IP); err != nil {
		log.Printf("Failed to release IP: %v", err)
		// Continue with cleanup
	}

	log.Printf("Released IP %s", record.IP)

	// Delete allocation record
	if err := a.consulManager.DeleteAllocationRecord(allocID); err != nil {
		log.Printf("Failed to delete allocation record: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete allocation record: %v", err))
		return
	}

	log.Printf("Deleted allocation record for alloc_id=%s", allocID)

	// Send success response
	resp := &client.DeallocateResponse{Success: true}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleHealth handles health check requests.
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// sendError sends an error response.
func (a *Agent) sendError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	resp := &client.AllocateResponse{
		Success: false,
		Error:   message,
	}

	json.NewEncoder(w).Encode(resp)
}

// cleanupStaleAllocations cleans up allocations from previous agent runs
// that may not have been properly deallocated.
func (a *Agent) cleanupStaleAllocations() error {
	log.Printf("Checking for stale allocations...")

	// Get all allocation records from Consul
	records, err := a.consulManager.GetAllAllocations()
	if err != nil {
		return fmt.Errorf("failed to get allocations: %w", err)
	}

	if len(records) == 0 {
		log.Printf("No allocations found in Consul")
		return nil
	}

	log.Printf("Found %d allocation(s) in Consul, checking if they are still active...", len(records))

	cleaned := 0
	for _, record := range records {
		// Check if allocation still exists in Nomad
		_, err := a.nomadClient.GetTaskMetadata(record.AllocID)
		if err != nil {
			// Allocation doesn't exist in Nomad, clean it up
			log.Printf("Cleaning up stale allocation %s (IP: %s)", record.AllocID, record.IP)

			// Stop Nebula instance if running
			_ = a.nebulaManager.StopInstance(record.AllocID)

			// Release IP
			if err := a.consulManager.ReleaseIP(record.IP); err != nil {
				log.Printf("Warning: failed to release IP %s: %v", record.IP, err)
			}

			// Delete allocation record
			if err := a.consulManager.DeleteAllocationRecord(record.AllocID); err != nil {
				log.Printf("Warning: failed to delete allocation record %s: %v", record.AllocID, err)
			} else {
				cleaned++
			}
		}
	}

	if cleaned > 0 {
		log.Printf("Cleaned up %d stale allocation(s)", cleaned)
	} else {
		log.Printf("All allocations are still active")
	}

	return nil
}

// waitForNomad waits for Nomad to be available with a timeout.
func (a *Agent) waitForNomad(timeout time.Duration) error {
	log.Printf("Waiting for Nomad to be available (timeout: %v)...", timeout)

	deadline := time.Now().Add(timeout)
	attempt := 1

	for time.Now().Before(deadline) {
		// Try to list nodes as a health check
		_, _, err := a.nomadClient.client.Nodes().List(nil)
		if err == nil {
			log.Printf("Nomad is available")
			return nil
		}

		if attempt == 1 {
			log.Printf("Nomad not available yet (attempt %d): %v", attempt, err)
		} else if attempt%5 == 0 {
			log.Printf("Still waiting for Nomad (attempt %d): %v", attempt, err)
		}

		attempt++
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("timeout waiting for Nomad after %v", timeout)
}

// getLocalNodeID attempts to determine the local Nomad node ID by matching IP addresses.
func (a *Agent) getLocalNodeID() (string, error) {
	// Get local IP addresses
	localIPs, err := getLocalIPAddresses()
	if err != nil {
		return "", fmt.Errorf("failed to get local IP addresses: %w", err)
	}

	// Get all nodes from Nomad API
	nodes, _, err := a.nomadClient.client.Nodes().List(nil)
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	// Find node matching our IP address
	for _, node := range nodes {
		// Get full node info to access addresses
		nodeAddr := node.Address
		if localIPs[nodeAddr] {
			log.Printf("Matched local node by IP address: %s (node ID: %s)", nodeAddr, node.ID)
			return node.ID, nil
		}
	}

	return "", fmt.Errorf("no node found matching local IP addresses: %v", getIPList(localIPs))
}

// getLocalIPAddresses returns a map of all local IP addresses.
func getLocalIPAddresses() (map[string]bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get interface addresses: %w", err)
	}

	localIPs := make(map[string]bool)
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}

		if ip != nil {
			localIPs[ip.String()] = true
		}
	}

	return localIPs, nil
}

// getIPList converts IP map to list for logging.
func getIPList(ips map[string]bool) []string {
	list := make([]string, 0, len(ips))
	for ip := range ips {
		list = append(list, ip)
	}
	return list
}

// getLocalAllocations returns all allocation records for this node only.
func (a *Agent) getLocalAllocations() ([]*client.AllocationRecord, error) {
	// Get all allocations from Consul
	allRecords, err := a.consulManager.GetAllAllocations()
	if err != nil {
		return nil, fmt.Errorf("failed to get allocations from Consul: %w", err)
	}

	if len(allRecords) == 0 {
		return nil, nil
	}

	localNodeId, err := a.getLocalNodeID()
	if err != nil {
		return nil, fmt.Errorf("failed to get local node ID: %w", err)
	}

	// Get running allocations on this node from Nomad
	var nomadAllocIDs map[string]bool
	allocIDs, err := a.nomadClient.GetNodeAllocations(localNodeId)
	if err != nil {
		log.Printf("Warning: failed to get node allocations from Nomad: %v", err)
		// Fall back to hostname filtering only
	} else {
		nomadAllocIDs = make(map[string]bool)
		for _, id := range allocIDs {
			nomadAllocIDs[id] = true
		}
	}

	// Filter records for this node
	localRecords := make([]*client.AllocationRecord, 0)
	for _, record := range allRecords {
		if record.NodeID != localNodeId {
			continue
		}

		// If we have Nomad data, also verify allocation is running on this node
		if nomadAllocIDs != nil {
			if !nomadAllocIDs[record.AllocID] {
				log.Printf("Allocation %s in Consul but not running on this node (skipping)", record.AllocID)
				continue
			}
		}

		localRecords = append(localRecords, record)
	}

	return localRecords, nil
}

// restartLocalInstances restarts Nebula instances for allocations on this node.
func (a *Agent) restartLocalInstances() error {
	log.Printf("Checking for Nebula instances to restart on this node...")

	// Get allocations for this node only
	records, err := a.getLocalAllocations()
	if err != nil {
		return fmt.Errorf("failed to get local allocations: %w", err)
	}

	if len(records) == 0 {
		log.Printf("No local allocations found to restart")
		return nil
	}

	log.Printf("Found %d local allocation(s), checking which need restart...", len(records))

	restarted := 0
	skipped := 0
	failed := 0

	for _, record := range records {
		// Check if already running
		if _, exists := a.nebulaManager.GetInstance(record.AllocID); exists {
			log.Printf("Nebula instance for %s already running, skipping", record.AllocID)
			skipped++
			continue
		}

		// Check if network namespace still exists
		if !namespaceExists(record.NetNS) {
			log.Printf("Network namespace %s for allocation %s no longer exists, skipping",
				record.NetNS, record.AllocID)
			skipped++
			continue
		}

		// Get task metadata from Nomad for config
		metadata, err := a.nomadClient.GetTaskMetadata(record.AllocID)
		if err != nil {
			log.Printf("Failed to get metadata for %s: %v (allocation may be gone)", record.AllocID, err)
			skipped++
			continue
		}

		// Get CA cert
		caCertPEM, err := a.certSigner.GetCACertificate()
		if err != nil {
			log.Printf("Failed to get CA cert for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		// Sign new certificate (always needed on restart since we don't persist certs)
		certName := fmt.Sprintf("%s.%s.%s", metadata.TaskName, metadata.TaskGroup, metadata.JobID)
		certPEM, keyPEM, err := a.certSigner.SignCertificate(
			record.IP, metadata.Roles, certName, a.config.CertTTL)
		if err != nil {
			log.Printf("Failed to sign certificate for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		// Restart Nebula instance
		err = a.nebulaManager.StartInstance(
			record.AllocID,
			record.IP,
			certPEM,
			keyPEM,
			caCertPEM,
			metadata.NebulaConfig,
			record.NetNS,
		)

		if err != nil {
			log.Printf("Failed to restart Nebula for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		restarted++
		log.Printf("Restarted Nebula instance for allocation %s (IP: %s)", record.AllocID, record.IP)
	}

	log.Printf("Restart summary: %d restarted, %d skipped, %d failed", restarted, skipped, failed)

	if failed > 0 {
		return fmt.Errorf("%d instances failed to restart", failed)
	}

	return nil
}

// namespaceExists checks if a network namespace path exists.
func namespaceExists(netnsPath string) bool {
	_, err := os.Stat(netnsPath)
	return err == nil
}

// startCertRotationWorker starts a background goroutine that periodically
// checks and rotates certificates that are close to expiry.
func (a *Agent) startCertRotationWorker() {
	go func() {
		// Run every 20% of TTL
		ticker := time.NewTicker(a.config.CertTTL / 5)
		defer ticker.Stop()

		log.Printf("Certificate rotation worker started (checking every %v)", a.config.CertTTL/5)

		for range ticker.C {
			if err := a.rotateCertificates(); err != nil {
				log.Printf("Certificate rotation error: %v", err)
			}
		}
	}()
}

// rotateCertificates checks all local allocations and rotates certificates
// that are close to expiry (< 25% TTL remaining).
func (a *Agent) rotateCertificates() error {
	log.Printf("Running certificate rotation check...")

	// Get local allocations only
	records, err := a.getLocalAllocations()
	if err != nil {
		return fmt.Errorf("failed to get local allocations: %w", err)
	}

	if len(records) == 0 {
		log.Printf("No local allocations to check for rotation")
		return nil
	}

	rotated := 0
	skipped := 0
	failed := 0

	for _, record := range records {
		// Read certificate from running instance's config
		nebulaCert, err := a.nebulaManager.GetCertFromConfig(record.AllocID)
		if err != nil {
			log.Printf("Failed to read cert for %s: %v, skipping rotation", record.AllocID, err)
			skipped++
			continue
		}

		// Check if cert needs rotation (< 25% TTL remaining)
		certExpiry := nebulaCert.NotAfter()
		timeRemaining := time.Until(certExpiry)
		rotationThreshold := a.config.CertTTL / 4

		if timeRemaining > rotationThreshold {
			skipped++
			continue
		}

		log.Printf("Rotating certificate for %s (expires in %v)", record.AllocID, timeRemaining)

		if err := a.rotateSingleCert(record); err != nil {
			log.Printf("Failed to rotate cert for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		rotated++
	}

	log.Printf("Rotation summary: %d rotated, %d skipped, %d failed", rotated, skipped, failed)

	if failed > 0 {
		return fmt.Errorf("%d certificates failed to rotate", failed)
	}

	return nil
}

// rotateSingleCert rotates the certificate for a single allocation using config reload.
func (a *Agent) rotateSingleCert(record *client.AllocationRecord) error {
	// Sign new certificate
	metadata, err := a.nomadClient.GetTaskMetadata(record.AllocID)
	if err != nil {
		return fmt.Errorf("failed to get metadata: %w", err)
	}

	certName := fmt.Sprintf("%s.%s.%s", metadata.TaskName, metadata.TaskGroup, metadata.JobID)
	newCertPEM, newKeyPEM, err := a.certSigner.SignCertificate(
		record.IP,
		metadata.Roles,
		certName,
		a.config.CertTTL,
	)
	if err != nil {
		return fmt.Errorf("failed to sign new certificate: %w", err)
	}

	// Get CA cert
	caCertPEM, err := a.certSigner.GetCACertificate()
	if err != nil {
		return fmt.Errorf("failed to get CA certificate: %w", err)
	}

	// Generate new config string with rotated cert
	newConfigString, err := a.nebulaManager.GenerateConfigString(
		newCertPEM,
		newKeyPEM,
		caCertPEM,
		metadata.NebulaConfig,
		record.IP,
	)
	if err != nil {
		return fmt.Errorf("failed to generate new config: %w", err)
	}

	// Reload config in running instance (zero-downtime rotation)
	if err := a.nebulaManager.ReloadConfig(record.AllocID, newConfigString); err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	newExpiry := time.Now().Add(a.config.CertTTL)
	log.Printf("Successfully rotated certificate for %s (new expiry: %v)", record.AllocID, newExpiry)

	return nil
}
