package agent

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/worker"
	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"github.com/slackhq/nebula/cert"
)

// NebulaManager manages Nebula worker processes via systemd.
type NebulaManager struct {
	nebulaConfig     string
	workerBinaryPath string
	instances        map[string]*NebulaInstance
	instancesLock    sync.RWMutex
	systemdConn      *dbus.Conn
	logger           *logrus.Entry
}

// NebulaInstance represents a running Nebula worker process.
type NebulaInstance struct {
	AllocID  string
	UnitName string // systemd unit name
}

// NewNebulaManager creates a new Nebula instance manager.
func NewNebulaManager(nebulaConfig, workerBinaryPath string, logger *logrus.Entry) *NebulaManager {
	return &NebulaManager{
		nebulaConfig:     nebulaConfig,
		workerBinaryPath: workerBinaryPath,
		instances:        make(map[string]*NebulaInstance),
		logger:           logger.WithField("component", "nebula-manager"),
	}
}

// StartInstance starts a Nebula worker process for the given allocation using systemd.
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

	// Get systemd connection
	conn, err := nm.getSystemdConnection()
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %w", err)
	}

	// Create transient unit with network namespace
	unitName := worker.GetUnitName(allocID)
	socketPath := worker.GetSocketPath(allocID)

	// Stop any existing unit with this namespace
	if err := nm.stopExistingUnit(conn, unitName); err != nil {
		nm.logger.Warnf("failed to stop existing unit %s: %v", unitName, err)
		// Continue anyway - we'll try to create the unit
	}

	properties := []dbus.Property{
		dbus.PropExecStart([]string{nm.workerBinaryPath}, false),
		dbusProperty("NetworkNamespacePath", netns),
		dbusProperty("Environment", []string{
			"ALLOC_ID=" + allocID,
			"NEBULA_SOCKET=" + socketPath,
			"NEBULA_CONFIG=" + configString,
		}),
		dbusProperty("Restart", "on-failure"),
	}

	// Start unit
	_, err = conn.StartTransientUnitContext(context.TODO(), unitName, "replace", properties, nil)
	if err != nil {
		return fmt.Errorf("failed to start systemd unit: %w", err)
	}

	nm.logger.Infof("Started systemd unit %s for allocation %s", unitName, allocID)

	// Create instance record
	instance := &NebulaInstance{
		AllocID:  allocID,
		UnitName: unitName,
	}

	nm.instances[allocID] = instance

	return nil
}

// getSystemdConnection gets or creates a systemd D-Bus connection.
func (nm *NebulaManager) getSystemdConnection() (*dbus.Conn, error) {
	if nm.systemdConn != nil {
		return nm.systemdConn, nil
	}

	conn, err := dbus.NewSystemdConnectionContext(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to systemd: %w", err)
	}

	nm.systemdConn = conn
	return conn, nil
}

// stopExistingUnit stops an existing systemd unit if it exists.
func (nm *NebulaManager) stopExistingUnit(conn *dbus.Conn, unitName string) error {
	// Check if unit exists by trying to get its properties
	units, err := conn.ListUnitsByNamesContext(context.TODO(), []string{unitName})
	if err != nil {
		return fmt.Errorf("failed to check unit status: %w", err)
	}

	if len(units) == 0 {
		return nil // Unit doesn't exist, nothing to stop
	}

	unit := units[0]

	if unit.LoadState == "not-found" {
		return nil // Unit doesn't exist
	}

	if unit.ActiveState == "active" || unit.ActiveState == "activating" {
		nm.logger.Infof("Stopping existing unit %s (state: %s)", unitName, unit.ActiveState)
		_, err := conn.StopUnitContext(context.TODO(), unitName, "replace", nil)
		if err != nil {
			return fmt.Errorf("failed to stop unit: %w", err)
		}
	}

	if err := conn.ResetFailedUnitContext(context.TODO(), unitName); err != nil {
		nm.logger.Warnf("failed to reset unit state: %v", err)
	}

	return nil
}

// StopInstance stops a Nebula worker process for the given allocation.
func (nm *NebulaManager) StopInstance(allocID string) error {
	nm.instancesLock.Lock()
	defer nm.instancesLock.Unlock()

	instance, exists := nm.instances[allocID]
	if !exists {
		return nil // Instance doesn't exist, nothing to do
	}

	// Get systemd connection
	conn, err := nm.getSystemdConnection()
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %w", err)
	}

	// Stop the systemd unit
	_, err = conn.StopUnitContext(context.TODO(), instance.UnitName, "replace", nil)
	if err != nil {
		nm.logger.Warnf("failed to stop systemd unit %s: %v", instance.UnitName, err)
		// Continue with cleanup even if stop fails
	}

	// Clean up socket file
	socketPath := worker.GetSocketPath(allocID)
	_ = os.RemoveAll(socketPath)

	delete(nm.instances, allocID)
	nm.logger.Infof("Stopped Nebula worker for allocation %s", allocID)

	return nil
}

// GetInstance retrieves an instance by allocation ID.
func (nm *NebulaManager) GetInstance(allocID string) (*NebulaInstance, bool) {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	instance, exists := nm.instances[allocID]
	return instance, exists
}

// StopAll stops all running Nebula worker processes (called on agent shutdown).
func (nm *NebulaManager) StopAll() {
	nm.instancesLock.Lock()
	defer nm.instancesLock.Unlock()

	nm.logger.Infof("Stopping all Nebula instances (%d running)", len(nm.instances))

	for allocID := range nm.instances {
		// Stop without holding the lock (StopInstance acquires it)
		// Make a copy of allocID for the goroutine
		id := allocID
		nm.instancesLock.Unlock()
		if err := nm.StopInstance(id); err != nil {
			nm.logger.Warnf("failed to stop instance %s: %v", id, err)
		}
		nm.instancesLock.Lock()
	}

	// Close systemd connection
	if nm.systemdConn != nil {
		nm.systemdConn.Close()
		nm.systemdConn = nil
	}

	nm.logger.Info("All Nebula instances stopped")
}

// generateInstanceConfigString generates a Nebula config string with inline certs.
// This combines the base config with job-specific overrides.
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

func dbusProperty(name string, v any) dbus.Property {
	return dbus.Property{
		Name:  name,
		Value: godbus.MakeVariant(v),
	}
}

// GenerateConfigString is a public wrapper for generateInstanceConfigString.
func (nm *NebulaManager) GenerateConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip string) (string, error) {
	return nm.generateInstanceConfigString(certPEM, keyPEM, caCertPEM, jobConfig, ip)
}

// ReloadConfig reloads the config for a running Nebula worker via socket RPC.
func (nm *NebulaManager) ReloadConfig(allocID, newConfigString string) error {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	_, exists := nm.instances[allocID]
	if !exists {
		return fmt.Errorf("instance not found")
	}

	// Send reload command to worker via socket
	socketPath := worker.GetSocketPath(allocID)
	client := worker.NewClient(socketPath)

	if err := client.Reload(newConfigString); err != nil {
		return fmt.Errorf("failed to reload config via RPC: %w", err)
	}

	nm.logger.Infof("Successfully reloaded config for allocation %s", allocID)
	return nil
}

// GetCertFromConfig reads the certificate from a worker's current config via RPC.
func (nm *NebulaManager) GetCertFromConfig(allocID string) (cert.Certificate, error) {
	nm.instancesLock.RLock()
	defer nm.instancesLock.RUnlock()

	_, exists := nm.instances[allocID]
	if !exists {
		return nil, fmt.Errorf("instance not found")
	}

	// Get config from worker via socket
	socketPath := worker.GetSocketPath(allocID)
	client := worker.NewClient(socketPath)

	configYAML, err := client.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config from worker: %w", err)
	}

	// Parse YAML config to extract cert
	configMap, err := parseYAML(configYAML)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	// Get pki.cert from config
	pkiInterface, ok := configMap["pki"]
	if !ok {
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
