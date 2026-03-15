package config

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Config represents the agent configuration.
type Config struct {
	SocketPath       string        `toml:"socket_path"`
	ConsulAddr       string        `toml:"consul_addr"`
	NomadAddr        string        `toml:"nomad_addr"`
	CACertPath       string        `toml:"ca_cert_path"`
	CAKeyPath        string        `toml:"ca_key_path"`
	NebulaConfigPath string        `toml:"nebula_config_path"`
	CertTTL          time.Duration `toml:"cert_ttl"`
	IPPool           IPPoolConfig  `toml:"ip_pool"`
}

// IPPoolConfig defines the IP allocation pool settings.
type IPPoolConfig struct {
	NetworkCIDR string `toml:"network_cidr"`
	RangeStart  string `toml:"range_start"`
	RangeEnd    string `toml:"range_end"`
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() *Config {
	return &Config{
		SocketPath:       "/var/run/nebula-cni.sock",
		ConsulAddr:       "127.0.0.1:8500",
		NomadAddr:        "http://127.0.0.1:4646",
		CACertPath:       "/etc/nebula-cni/ca.crt",
		CAKeyPath:        "/etc/nebula-cni/ca.key",
		NebulaConfigPath: "/etc/nebula-cni/nebula-config.yaml",
		CertTTL:          1 * time.Hour,
	}
}

// LoadConfig loads configuration from a TOML file, overriding defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Validate required fields
	if cfg.IPPool.NetworkCIDR == "" {
		return nil, fmt.Errorf("ip_pool.network_cidr is required")
	}
	if cfg.IPPool.RangeStart == "" {
		return nil, fmt.Errorf("ip_pool.range_start is required")
	}
	if cfg.IPPool.RangeEnd == "" {
		return nil, fmt.Errorf("ip_pool.range_end is required")
	}

	return cfg, nil
}
