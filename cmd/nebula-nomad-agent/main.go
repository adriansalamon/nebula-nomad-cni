package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/agent"
	"github.com/adriansalamon/nebula-nomad-cni/pkg/config"
	"github.com/adriansalamon/nebula-nomad-cni/pkg/version"
)

func main() {
	// Parse command line flags
	var (
		configPath  = flag.String("config", "/etc/nebula-cni/agent.toml", "Path to configuration file")
		showVersion = flag.Bool("version", false, "Show version and exit")
	)

	flag.Parse()

	if *showVersion {
		log.Printf("nebula-nomad-agent version %s (commit: %s)", version.Version, version.GitCommit)
		os.Exit(0)
	}

	log.Printf("Starting nebula-nomad-agent version %s", version.Version)

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create agent config from loaded config
	agentConfig := &agent.Config{
		SocketPath:   cfg.SocketPath,
		ConsulAddr:   cfg.ConsulAddr,
		NomadAddr:    cfg.NomadAddr,
		CACertPath:   cfg.CACertPath,
		CAKeyPath:    cfg.CAKeyPath,
		NebulaConfig: cfg.NebulaConfigPath,
		CertTTL:      cfg.CertTTL,
	}

	// Create agent
	ag, err := agent.NewAgent(agentConfig)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	// Initialize IP pool on first run
	if err := ag.InitializeIPPool(cfg.IPPool.NetworkCIDR, cfg.IPPool.RangeStart, cfg.IPPool.RangeEnd); err != nil {
		log.Printf("Note: IP pool initialization failed (may already exist): %v", err)
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
		log.Printf("Received signal: %v, shutting down...", sig)
		if err := ag.Stop(); err != nil {
			log.Printf("Error stopping agent: %v", err)
		}
	case err := <-errChan:
		log.Fatalf("Agent error: %v", err)
	}

	log.Println("Agent stopped")
}
