package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula"
	"github.com/slackhq/nebula/config"
	"go.yaml.in/yaml/v2"
)

// Worker runs a single Nebula instance and exposes control via Unix socket.
type Worker struct {
	allocID      string
	socketPath   string
	control      *nebula.Control
	nebulaConfig *config.C
	logger       *logrus.Logger
	listener     net.Listener
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// New creates a new worker instance.
func New(allocID, socketPath, configString string) (*Worker, error) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	logger = logger.WithField("alloc_id", allocID).Logger
	logger.SetOutput(os.Stdout)

	ctx, cancel := context.WithCancel(context.Background())

	nebulaConfig := config.NewC(logger)
	if err := nebulaConfig.LoadString(configString); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	return &Worker{
		allocID:      allocID,
		socketPath:   socketPath,
		nebulaConfig: nebulaConfig,
		logger:       logger,
		ctx:          ctx,
		cancel:       cancel,
	}, nil
}

// Run starts the Nebula instance and RPC socket server.
func (w *Worker) Run() error {
	// Start Nebula
	if err := w.startNebula(); err != nil {
		return fmt.Errorf("failed to start Nebula: %w", err)
	}

	// Start socket server
	if err := w.startSocketServer(); err != nil {
		w.control.Stop()
		return fmt.Errorf("failed to start socket server: %w", err)
	}

	w.logger.Infof("Worker running for allocation %s (socket: %s)", w.allocID, w.socketPath)

	// Wait for shutdown
	w.wg.Wait()

	return nil
}

// startNebula initializes and starts the Nebula instance.
func (w *Worker) startNebula() error {
	// Initialize Nebula (creates tun device in current netns)
	control, err := nebula.Main(w.nebulaConfig, false, "nebula-nomad", w.logger, nil)
	if err != nil {
		return fmt.Errorf("failed to initialize Nebula: %w", err)
	}

	if control == nil {
		return fmt.Errorf("nebula.Main returned nil control")
	}

	// Start Nebula's background workers (now safe - entire process is in correct netns)
	control.Start()

	w.control = control
	w.logger.Info("Nebula instance started successfully")

	return nil
}

// startSocketServer starts the Unix socket RPC server.
func (w *Worker) startSocketServer() error {
	// Remove stale socket if exists
	_ = os.RemoveAll(w.socketPath)

	listener, err := net.Listen("unix", w.socketPath)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}

	// Set permissions so agent can connect
	if err := os.Chmod(w.socketPath, 0666); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	w.listener = listener

	// Accept connections in background
	w.wg.Add(1)
	go w.acceptConnections()

	return nil
}

// acceptConnections handles incoming socket connections.
func (w *Worker) acceptConnections() {
	defer w.wg.Done()
	defer w.listener.Close()

	for {
		conn, err := w.listener.Accept()
		if err != nil {
			select {
			case <-w.ctx.Done():
				return
			default:
				w.logger.Errorf("Accept error: %v", err)
				continue
			}
		}

		// Handle connection in background
		go w.handleConnection(conn)
	}
}

// handleConnection handles a single RPC request.
func (w *Worker) handleConnection(conn net.Conn) {
	defer conn.Close()

	var req Request
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&req); err != nil {
		w.sendError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}

	var resp Response

	switch req.Command {
	case CommandPing:
		resp = Response{Success: true}

	case CommandGetConfig:
		yamlString, err := yaml.Marshal(w.nebulaConfig.Settings)
		if err != nil {
			resp = Response{Success: false, Error: err.Error()}
			break
		}

		resp = Response{
			Success: true,
			Data:    string(yamlString),
		}

	case CommandReload:
		if req.Data == "" {
			resp = Response{Success: false, Error: "reload requires config data"}
		} else if err := w.reloadConfig(req.Data); err != nil {
			resp = Response{Success: false, Error: err.Error()}
		} else {
			resp = Response{Success: true}
		}

	case CommandStop:
		resp = Response{Success: true}
		w.sendResponse(conn, resp)
		w.shutdown()
		return

	default:
		resp = Response{Success: false, Error: fmt.Sprintf("unknown command: %s", req.Command)}
	}

	w.sendResponse(conn, resp)
}

// reloadConfig reloads Nebula config without restart.
func (w *Worker) reloadConfig(newConfigString string) error {
	w.logger.Info("Reloading Nebula configuration")

	// Use Nebula's built-in config reload
	if err := w.nebulaConfig.ReloadConfigString(newConfigString); err != nil {
		return fmt.Errorf("config reload failed: %w", err)
	}

	w.logger.Info("Configuration reloaded successfully")
	return nil
}

// shutdown gracefully stops the worker.
func (w *Worker) shutdown() {
	w.logger.Info("Shutting down worker")

	// Stop Nebula
	if w.control != nil {
		w.control.Stop()
	}

	// Stop socket server
	w.cancel()
	if w.listener != nil {
		w.listener.Close()
	}

	// Clean up socket file
	_ = os.RemoveAll(w.socketPath)

	w.logger.Info("Worker stopped")
}

// sendResponse sends a JSON response to the client.
func (w *Worker) sendResponse(conn net.Conn, resp Response) {
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(resp); err != nil {
		w.logger.Errorf("Failed to send response: %v", err)
	}
}

// sendError sends an error response.
func (w *Worker) sendError(conn net.Conn, message string) {
	w.sendResponse(conn, Response{Success: false, Error: message})
}

// RunFromEnv is a convenience function that reads config from environment and runs worker.
func RunFromEnv() error {
	allocID := os.Getenv("ALLOC_ID")
	socketPath := os.Getenv("NEBULA_SOCKET")
	configString := os.Getenv("NEBULA_CONFIG")

	if allocID == "" || socketPath == "" || configString == "" {
		return fmt.Errorf("missing required environment variables (ALLOC_ID, NEBULA_SOCKET, NEBULA_CONFIG)")
	}

	worker, err := New(allocID, socketPath, configString)
	if err != nil {
		return fmt.Errorf("failed to create worker: %w", err)
	}

	return worker.Run()
}
