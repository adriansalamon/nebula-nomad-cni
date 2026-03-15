package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula"
	"github.com/slackhq/nebula/cert"
	"github.com/slackhq/nebula/config"
	"github.com/vishvananda/netns"
)

// NebulaManager manages Nebula instances.
type NebulaManager struct {
	nebulaConfig  string
	instances     map[string]*NebulaInstance
	instancesLock sync.RWMutex
}

// NebulaInstance represents a running Nebula instance.
type NebulaInstance struct {
	AllocID string
	control *nebula.Control
	cancel  context.CancelFunc
	logger  *logrus.Logger
	config  *config.C // Nebula config object (has Get methods to read config)
}

// NewNebulaManager creates a new Nebula instance manager.
func NewNebulaManager(nebulaConfig string) *NebulaManager {
	return &NebulaManager{
		nebulaConfig: nebulaConfig,
		instances:    make(map[string]*NebulaInstance),
	}
}

// StartInstance starts a Nebula instance for the given allocation.
func (nm *NebulaManager) StartInstance(allocID, ip, certPEM, keyPEM, caCertPEM, jobConfig, netns string) error {
	nm.instancesLock.Lock()
	defer nm.instancesLock.Unlock()

	// Check if instance already exists
	if _, exists := nm.instances[allocID]; exists {
		return fmt.Errorf("instance already exists for allocation %s", allocID)
	}

	// Generate instance config string with inline certs
	configString, err := nm.generateInstanceConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip)
	if err != nil {
		return fmt.Errorf("failed to generate instance config: %w", err)
	}

	// Create logger for this instance
	// Logs go to stdout (captured by systemd/journald) with allocation ID prefix
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Add allocation ID to all log entries
	logger = logger.WithField("alloc_id", allocID).Logger

	// Write to stdout so systemd captures it
	logger.SetOutput(os.Stdout)

	// Start Nebula in the network namespace
	ctx, cancel := context.WithCancel(context.Background())

	// Start the Nebula instance with config string
	control, nebulaConfig, err := nm.startNebulaInNamespace(ctx, netns, configString, logger)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to start Nebula in namespace: %w", err)
	}

	// Create instance record
	instance := &NebulaInstance{
		AllocID: allocID,
		control: control,
		cancel:  cancel,
		logger:  logger,
		config:  nebulaConfig,
	}

	nm.instances[allocID] = instance

	return nil
}

// startNebulaInNamespace starts a Nebula instance in the specified network namespace.
func (nm *NebulaManager) startNebulaInNamespace(ctx context.Context, netnsPath, configString string, logger *logrus.Logger) (*nebula.Control, *config.C, error) {
	// Open the network namespace file
	nsHandle, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open network namespace %s: %w", netnsPath, err)
	}
	defer nsHandle.Close()

	// Channel to return result from goroutine
	type result struct {
		control *nebula.Control
		config  *config.C
		err     error
	}
	resultCh := make(chan result, 1)

	// Start Nebula in a goroutine within the target network namespace
	go func() {
		// Lock goroutine to OS thread to maintain namespace affinity
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// Save current namespace to restore later
		origNs, err := netns.Get()
		if err != nil {
			resultCh <- result{err: fmt.Errorf("failed to get current namespace: %w", err)}
			return
		}
		defer origNs.Close()

		// Switch to target namespace
		if err := netns.Set(nsHandle); err != nil {
			resultCh <- result{err: fmt.Errorf("failed to set network namespace: %w", err)}
			return
		}
		defer netns.Set(origNs)

		// Load Nebula config from string
		c := config.NewC(logger)
		if err := c.LoadString(configString); err != nil {
			resultCh <- result{err: fmt.Errorf("failed to load config: %w", err)}
			return
		}

		// Initialize Nebula (creates tun device in current namespace)
		control, err := nebula.Main(c, false, "nebula-nomad", logger, nil)
		if err != nil {
			resultCh <- result{err: fmt.Errorf("failed to start Nebula: %w", err)}
			return
		}

		if control == nil {
			resultCh <- result{err: fmt.Errorf("nebula.Main returned nil control")}
			return
		}

		// Start Nebula's background workers
		control.Start()

		// Signal that Nebula has started successfully
		resultCh <- result{control: control, config: c}
	}()

	// Wait for Nebula to start
	res := <-resultCh
	if res.err != nil {
		return nil, nil, res.err
	}

	return res.control, res.config, nil
}

// StopInstance stops a Nebula instance for the given allocation.
func (nm *NebulaManager) StopInstance(allocID string) error {
	nm.instancesLock.Lock()
	defer nm.instancesLock.Unlock()

	instance, exists := nm.instances[allocID]
	if !exists {
		return nil // Instance doesn't exist, nothing to do
	}

	// Stop the Nebula control (this will unblock ShutdownBlock())
	if instance.control != nil {
		instance.control.Stop()
	}

	// Cancel the context for cleanup
	instance.cancel()

	// Remove from instances map
	delete(nm.instances, allocID)

	return nil
}

// GetInstance retrieves an instance by allocation ID.
func (nm *NebulaManager) GetInstance(allocID string) (*NebulaInstance, bool) {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	instance, exists := nm.instances[allocID]
	return instance, exists
}

// StopAll stops all running Nebula instances (called on agent shutdown).
func (nm *NebulaManager) StopAll() {
	nm.instancesLock.Lock()
	defer nm.instancesLock.Unlock()

	log.Printf("Stopping all Nebula instances (%d running)", len(nm.instances))

	for allocID := range nm.instances {
		// Stop without holding the lock (StopInstance acquires it)
		// Make a copy of allocID for the goroutine
		id := allocID
		nm.instancesLock.Unlock()
		if err := nm.StopInstance(id); err != nil {
			log.Printf("Warning: failed to stop instance %s: %v", id, err)
		}
		nm.instancesLock.Lock()
	}

	log.Printf("All Nebula instances stopped")
}

// generateInstanceConfigString generates a Nebula config string with inline certs.
// This combines the base cluster config with job-specific overrides.
func (nm *NebulaManager) generateInstanceConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip string) (string, error) {
	// Start with base config if provided
	configMap := map[string]any{}
	configMap["pki"] = map[string]any{
		"ca": caCertPEM,
	}

	if nm.nebulaConfig != "" {
		baseConfigData, err := os.ReadFile(nm.nebulaConfig)
		if err != nil {
			return "", fmt.Errorf("failed to read base Nebula config: %w", err)
		}

		userConfigMap, err := parseYAML(string(baseConfigData))
		if err != nil {
			return "", fmt.Errorf("failed to parse base config: %w", err)
		}

		configMap = mergeConfigs(configMap, userConfigMap)
	}

	// Parse job-specific config if provided
	if jobConfig != "" {
		jobConfigMap, err := parseYAML(jobConfig)
		if err != nil {
			return "", fmt.Errorf("failed to parse job config: %w", err)
		}

		// Merge job config into base config (highest priority)
		configMap = mergeConfigs(configMap, jobConfigMap)
	}

	pkiMap := map[string]any{
		"pki": map[string]any{
			"cert": certPEM,
			"key":  keyPEM,
		}}

	// Inline the PKI certs (this overrides any pki config)
	configMap = mergeConfigs(configMap, pkiMap)

	// Convert to YAML string
	finalConfig, err := toYAML(configMap)
	if err != nil {
		return "", fmt.Errorf("failed to marshal final config: %w", err)
	}

	return finalConfig, nil
}

// GenerateConfigString is a public wrapper for generateInstanceConfigString.
func (nm *NebulaManager) GenerateConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip string) (string, error) {
	return nm.generateInstanceConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip)
}

// ReloadConfig reloads the config for a running Nebula instance using ReloadConfigString.
func (nm *NebulaManager) ReloadConfig(allocID, newConfigString string) error {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	instance, exists := nm.instances[allocID]
	if !exists {
		return fmt.Errorf("instance not found")
	}

	// Use Nebula's ReloadConfigString to reload config without restarting
	if err := instance.config.ReloadConfigString(newConfigString); err != nil {
		return fmt.Errorf("failed to reload config string: %w", err)
	}

	log.Printf("Successfully reloaded config for allocation %s", allocID)
	return nil
}

// GetCertFromConfig reads the certificate from an allocation's running config.
func (nm *NebulaManager) GetCertFromConfig(allocID string) (cert.Certificate, error) {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	instance, exists := nm.instances[allocID]
	if !exists {
		return nil, fmt.Errorf("instance not found")
	}

	// Get pki.cert from config using config.Get
	pkiInterface := instance.config.Get("pki")
	if pkiInterface == nil {
		return nil, fmt.Errorf("no pki section in config")
	}

	pki, ok := pkiInterface.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("pki section is not a map")
	}

	certInterface, ok := pki["cert"]
	if !ok {
		return nil, fmt.Errorf("no cert in pki section")
	}

	certPEM, ok := certInterface.(string)
	if !ok {
		return nil, fmt.Errorf("cert is not a string")
	}

	// Parse certificate
	nebulaCert, _, err := cert.UnmarshalCertificateFromPEM([]byte(certPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse Nebula certificate: %w", err)
	}

	return nebulaCert, nil
}
