package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/agent"
	"github.com/adriansalamon/nebula-nomad-cni/pkg/config"
	"github.com/adriansalamon/nebula-nomad-cni/pkg/version"
	"github.com/sirupsen/logrus"
)

func main() {
	// Parse command line flags
	var (
		configPath  = flag.String("config", "/etc/nebula-cni/agent.toml", "Path to configuration file")
		showVersion = flag.Bool("version", false, "Show version and exit")
	)

	flag.Parse()

	// Initialize logger for main
	level, _ := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if level == 0 {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	if *showVersion {
		logrus.Infof("nebula-nomad-agent version %s (commit: %s)", version.Version, version.GitCommit)
		os.Exit(0)
	}

	logrus.Infof("Starting nebula-nomad-agent version %s", version.Version)

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logrus.Fatalf("Failed to load config: %v", err)
	}

	// Create agent config from loaded config
	agentConfig := &agent.Config{
		SocketPath:       cfg.SocketPath,
		ConsulAddr:       cfg.ConsulAddr,
		NomadAddr:        cfg.NomadAddr,
		CACertPath:       cfg.CACertPath,
		CAKeyPath:        cfg.CAKeyPath,
		NebulaConfig:     cfg.NebulaConfigPath,
		WorkerBinaryPath: cfg.WorkerBinaryPath,
		CertTTL:          cfg.CertTTL,
	}

	// Create appropriate signer
	var signer agent.Signer
	switch cfg.SignerType {
	case "vault":
		logrus.Infof("Using Vault signer at %s (mount: %s)", cfg.Vault.Addr, cfg.Vault.Mount)
		signer, err = agent.NewVaultSigner(
			cfg.Vault.Addr,
			cfg.Vault.Mount,
			cfg.Vault.RoleID,
			cfg.Vault.SecretPath,
		)
		if err != nil {
			logrus.Fatalf("Failed to create Vault signer: %v", err)
		}
	default:
		logrus.Infof("Using local signer (CA cert: %s)", cfg.CACertPath)
		signer, err = agent.NewLocalSigner(cfg.CACertPath, cfg.CAKeyPath)
		if err != nil {
			logrus.Fatalf("Failed to create local signer: %v", err)
		}
	}

	// Create agent
	ag, err := agent.NewAgent(agentConfig, signer)
	if err != nil {
		logrus.Fatalf("Failed to create agent: %v", err)
	}

	// Initialize IP pool on first run
	if err := ag.InitializeIPPool(cfg.IPPool.NetworkCIDR, cfg.IPPool.RangeStart, cfg.IPPool.RangeEnd); err != nil {
		logrus.Debugf("IP pool initialization skipped (may already exist): %v", err)
	}

	// Handle signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start agent in goroutine
	errChan := make(chan error, 1)
	go func() {
		if err := ag.Start(); err != nil {
			errChan <- err
		}
	}()

	// Wait for signal or error
	select {
	case sig := <-sigChan:
		logrus.Infof("Received signal: %v, shutting down...", sig)
		if err := ag.Stop(); err != nil {
			logrus.Errorf("Error stopping agent: %v", err)
		}
	case err := <-errChan:
		logrus.Fatalf("Agent error: %v", err)
	}

	logrus.Info("Agent stopped")
}
