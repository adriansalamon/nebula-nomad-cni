package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/client"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

// Config holds the configuration for the agent.
type Config struct {
	SocketPath       string
	ConsulAddr       string
	NomadAddr        string
	CACertPath       string
	CAKeyPath        string
	NebulaConfig     string
	WorkerBinaryPath string
	CertTTL          time.Duration
}

// Agent is the main agent service that handles IP allocation and certificate signing.
type Agent struct {
	config        *Config
	consulManager *ConsulManager
	nomadClient   *NomadClient
	certSigner    Signer
	nebulaManager *NebulaManager
	logger        *logrus.Entry
	httpServer    *http.Server
	listener      net.Listener
}

func NewAgent(config *Config, signer Signer) (*Agent, error) {
	// Create Consul manager
	consulManager, err := NewConsulManager(config.ConsulAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create Consul manager: %w", err)
	}

	nomadClient, err := NewNomadClient(config.NomadAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create Nomad client: %w", err)
	}

	logger := logrus.New()
	// Set level from env or default to Info
	level, _ := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if level == 0 {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	agentLogger := logger.WithField("component", "agent")

	nebulaManager := NewNebulaManager(config.NebulaConfig, config.WorkerBinaryPath, agentLogger)

	return &Agent{
		config:        config,
		consulManager: consulManager,
		nomadClient:   nomadClient,
		certSigner:    signer,
		nebulaManager: nebulaManager,
		logger:        agentLogger,
	}, nil
}

// Start starts the agent HTTP server on the unix socket.
func (a *Agent) Start() error {
	// Clean up stale allocations on startup
	if err := a.cleanupStaleAllocations(); err != nil {
		a.logger.Warnf("failed to cleanup stale allocations: %v", err)
		// Don't fail startup, just log warning
	}

	// Restart active Nebula instances for allocations on this node
	if err := a.restartLocalInstances(); err != nil {
		a.logger.Warnf("failed to restart local instances: %v", err)
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

	a.logger.Infof("Agent listening on %s", a.config.SocketPath)

	// Start certificate rotation worker
	a.startCertRotationWorker()

	// Start background cleanup worker (runs every 1 hour)
	a.startCleanupWorker()

	// Start serving
	if err := a.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}

	return nil
}

// Stop gracefully stops the agent and all running Nebula instances.
func (a *Agent) Stop() error {
	a.logger.Info("Stopping agent...")

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

	a.logger.Infof("Allocate request for alloc_id=%s", req.AllocID)

	// Validate request
	if req.AllocID == "" || req.NetNS == "" {
		a.sendError(w, http.StatusBadRequest, "alloc_id and netns are required")
		return
	}

	// Query Nomad API for task metadata
	metadata, err := a.nomadClient.GetTaskMetadata(req.AllocID)
	if err != nil {
		a.logger.Errorf("Failed to get task metadata from Nomad: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get task metadata: %v", err))
		return
	}

	// Check if allocation already exists
	existing, err := a.consulManager.GetAllocationRecord(req.AllocID)
	if err != nil {
		a.logger.Errorf("Error checking existing allocation: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to check existing allocation: %v", err))
		return
	}

	if existing != nil {
		a.logger.Warnf("Allocation %s already exists with IP %s", req.AllocID, existing.IP)
		a.sendError(w, http.StatusConflict, "allocation already exists")
		return
	}

	// Allocate IP from Consul (already includes correct subnet mask from pool's NetworkCIDR)
	ip, err := a.consulManager.AllocateIP()
	if err != nil {
		a.logger.Errorf("Failed to allocate IP: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to allocate IP: %v", err))
		return
	}

	// Get CA cert for inlining
	caCertPEM, err := a.certSigner.GetCACertificate()
	if err != nil {
		a.logger.Errorf("Failed to get CA certificate: %v", err)
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get CA certificate: %v", err))
		return
	}

	// Sign certificate
	certName := fmt.Sprintf("%s.%s.%s", metadata.TaskName, metadata.TaskGroup, metadata.JobID)
	certPEM, keyPEM, err := a.certSigner.SignCertificate(ip, metadata.Roles, certName, a.config.CertTTL)
	if err != nil {
		a.logger.Errorf("Failed to sign certificate: %v", err)
		// Release IP since we failed
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to sign certificate: %v", err))
		return
	}

	// Start Nebula instance with job-specific config
	if err := a.nebulaManager.StartInstance(req.AllocID, ip, certPEM, keyPEM, caCertPEM, metadata.NebulaConfig, req.NetNS); err != nil {
		a.logger.Errorf("Failed to start Nebula instance: %v", err)
		// Release IP and clean up
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to start Nebula instance: %v", err))
		return
	}

	// Store allocation record in Consul (minimal state - Nomad API is source of truth)
	record := &client.AllocationRecord{
		AllocID: req.AllocID,
		IP:      ip,
		NodeID:  metadata.NodeID,
		NetNS:   req.NetNS,
	}

	if err := a.consulManager.StoreAllocationRecord(record); err != nil {
		a.logger.Errorf("Failed to store allocation record: %v", err)
		// Clean up Nebula instance and IP
		_ = a.nebulaManager.StopInstance(req.AllocID)
		_ = a.consulManager.ReleaseIP(ip)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to store allocation record: %v", err))
		return
	}

	a.logger.Infof("Successfully allocated %s for %s", ip, req.AllocID)

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

	a.logger.Infof("Deallocate request for alloc_id=%s", allocID)

	if allocID == "" {
		a.sendError(w, http.StatusBadRequest, "alloc_id is required")
		return
	}

	// Get allocation record to find IP
	record, err := a.consulManager.GetAllocationRecord(allocID)
	if err != nil {
		a.logger.Errorf("Error getting allocation record: %v", err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get allocation record: %v", err))
		return
	}

	if record == nil {
		a.logger.Warnf("Allocation %s not found during deallocate", allocID)
		// Not found, but that's okay - consider it success
		resp := &client.DeallocateResponse{Success: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Stop Nebula instance
	if err := a.nebulaManager.StopInstance(allocID); err != nil {
		a.logger.Errorf("Failed to stop Nebula instance: %v", err)
		// Continue with cleanup even if this fails
	}

	// Release IP
	if err := a.consulManager.ReleaseIP(record.IP); err != nil {
		a.logger.Errorf("Failed to release IP %s: %v", record.IP, err)
		// Continue with cleanup
	}

	a.logger.Infof("Successfully deallocated %s for %s", record.IP, allocID)

	// Delete allocation record
	if err := a.consulManager.DeleteAllocationRecord(allocID); err != nil {
		a.logger.Errorf("Failed to delete allocation record for %s: %v", allocID, err)
		a.sendError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete allocation record: %v", err))
		return
	}

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
	a.logger.Debug("Performing full state cleanup check...")

	// 1. Clean up dangling records (Consul record exists but Nomad task is gone)
	records, err := a.consulManager.GetAllAllocations()
	if err != nil {
		return fmt.Errorf("failed to get allocations: %w", err)
	}

	recordIPs := make(map[string]bool)
	if len(records) > 0 {
		a.logger.Debugf("Checking %d allocation records for dangling tasks...", len(records))
		for _, record := range records {
			recordIPs[record.IP] = true
			// Check if allocation still exists in Nomad
			_, err := a.nomadClient.GetTaskMetadata(record.AllocID)
			if err != nil {
				a.logger.Infof("Cleaning up dangling allocation %s (IP: %s)", record.AllocID, record.IP)
				_ = a.nebulaManager.StopInstance(record.AllocID)
				_ = a.consulManager.ReleaseIP(record.IP)
				_ = a.consulManager.DeleteAllocationRecord(record.AllocID)
			}
		}
	}

	// 2. Clean up orphaned IPs (Pool says allocated, but no Consul record exists)
	pool, err := a.consulManager.GetIPPool()
	if err != nil {
		return fmt.Errorf("failed to get IP pool: %w", err)
	}

	orphanedCount := 0
	for _, ip := range pool.Allocated {
		if !recordIPs[ip] {
			a.logger.Infof("Reclaiming orphaned IP %s (leaked in pool)", ip)
			if err := a.consulManager.ReleaseIP(ip); err != nil {
				a.logger.Warnf("failed to release orphaned IP %s: %v", ip, err)
			} else {
				orphanedCount++
			}
		}
	}

	if orphanedCount > 0 {
		a.logger.Infof("Successfully reclaimed %d orphaned IP(s)", orphanedCount)
	}

	a.logger.Debug("Cleanup check complete")
	return nil
}

// startCleanupWorker starts a background goroutine that periodically
// cleans up stale allocations and leaked IPs.
func (a *Agent) startCleanupWorker() {
	go func() {
		// Run every 1 hour
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			if err := a.cleanupStaleAllocations(); err != nil {
				a.logger.Errorf("Periodic cleanup error: %v", err)
			}
		}
	}()
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
			a.logger.Infof("Matched local node by IP address: %s (node ID: %s)", nodeAddr, node.ID)
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
		a.logger.Warnf("failed to get node allocations from Nomad: %v", err)
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
				a.logger.Debugf("Allocation %s in Consul but not running on this node (skipping)", record.AllocID)
				continue
			}
		}

		localRecords = append(localRecords, record)
	}

	return localRecords, nil
}

// restartLocalInstances restarts Nebula instances for allocations on this node.
func (a *Agent) restartLocalInstances() error {
	a.logger.Debug("Checking for Nebula instances to restart on this node...")

	// Get allocations for this node only
	records, err := a.getLocalAllocations()
	if err != nil {
		return fmt.Errorf("failed to get local allocations: %w", err)
	}

	if len(records) == 0 {
		a.logger.Debug("No local allocations found to restart")
		return nil
	}

	a.logger.Infof("Found %d local allocation(s), checking which need restart...", len(records))

	restarted := 0
	skipped := 0
	failed := 0

	for _, record := range records {
		// Check if already running
		if _, exists := a.nebulaManager.GetInstance(record.AllocID); exists {
			a.logger.Debugf("Nebula instance for %s already running, skipping", record.AllocID)
			skipped++
			continue
		}

		// Check if network namespace still exists
		if !namespaceExists(record.NetNS) {
			a.logger.Warnf("Network namespace %s for allocation %s no longer exists, skipping", record.NetNS, record.AllocID)
			skipped++
			continue
		}

		// Get task metadata from Nomad for config
		metadata, err := a.nomadClient.GetTaskMetadata(record.AllocID)
		if err != nil {
			a.logger.Warnf("Failed to get metadata for %s: %v (allocation may be gone)", record.AllocID, err)
			skipped++
			continue
		}

		// Get CA cert
		caCertPEM, err := a.certSigner.GetCACertificate()
		if err != nil {
			a.logger.Errorf("Failed to get CA cert for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		// Sign new certificate (always needed on restart since we don't persist certs)
		certName := fmt.Sprintf("%s.%s.%s", metadata.TaskName, metadata.TaskGroup, metadata.JobID)
		certPEM, keyPEM, err := a.certSigner.SignCertificate(
			record.IP, metadata.Roles, certName, a.config.CertTTL)
		if err != nil {
			a.logger.Errorf("Failed to sign certificate for %s: %v", record.AllocID, err)
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
			a.logger.Errorf("Failed to restart Nebula for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		restarted++
		a.logger.Infof("Restarted Nebula instance for allocation %s (IP: %s)", record.AllocID, record.IP)
	}

	a.logger.Infof("Restart summary: %d restarted, %d skipped, %d failed", restarted, skipped, failed)

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

		a.logger.Infof("Certificate rotation worker started (checking every %v)", a.config.CertTTL/5)

		for range ticker.C {
			if err := a.rotateCertificates(); err != nil {
				a.logger.Errorf("Certificate rotation error: %v", err)
			}
		}
	}()
}

// rotateCertificates checks all local allocations and rotates certificates
// that are close to expiry (< 25% TTL remaining).
func (a *Agent) rotateCertificates() error {
	a.logger.Debug("Running certificate rotation check...")

	// Get local allocations only
	records, err := a.getLocalAllocations()
	if err != nil {
		return fmt.Errorf("failed to get local allocations: %w", err)
	}

	if len(records) == 0 {
		a.logger.Debug("No local allocations to check for rotation")
		return nil
	}

	rotated := 0
	skipped := 0
	failed := 0

	for _, record := range records {
		// Read certificate from running instance's config
		nebulaCert, err := a.nebulaManager.GetCertFromConfig(record.AllocID)
		if err != nil {
			a.logger.Warnf("Failed to read cert for %s: %v, skipping rotation", record.AllocID, err)
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

		a.logger.Infof("Rotating certificate for %s (expires in %v)", record.AllocID, timeRemaining)

		if err := a.rotateSingleCert(record); err != nil {
			a.logger.Errorf("Failed to rotate cert for %s: %v", record.AllocID, err)
			failed++
			continue
		}

		rotated++
	}

	a.logger.Infof("Rotation summary: %d rotated, %d skipped, %d failed", rotated, skipped, failed)

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
	a.logger.Infof("Successfully rotated certificate for %s (new expiry: %v)", record.AllocID, newExpiry)

	return nil
}
