package main

import (
	"flag"
	"log"
	"os"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/version"
	"github.com/adriansalamon/nebula-nomad-cni/pkg/worker"
)

func main() {
	var showVersion = flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		log.Printf("nebula-nomad-worker version %s (commit: %s)", version.Version, version.GitCommit)
		os.Exit(0)
	}

	log.Printf("Starting nebula-nomad-worker version %s", version.Version)

	// Run worker (reads config from environment variables)
	if err := worker.RunFromEnv(); err != nil {
		log.Fatalf("Worker failed: %v", err)
	}
}
