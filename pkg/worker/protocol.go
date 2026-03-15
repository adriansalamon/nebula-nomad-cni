package worker

// Command types for worker RPC.
const (
	CommandGetConfig = "GET_CONFIG"
	CommandReload    = "RELOAD"
	CommandStop      = "STOP"
	CommandPing      = "PING"
)

// Request sent to worker via Unix socket.
type Request struct {
	Command string `json:"command"` // GET_CONFIG, RELOAD, STOP, PING
	Data    string `json:"data"`    // For RELOAD: new config YAML
}

// Response from worker.
type Response struct {
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`  // For GET_CONFIG: current config YAML
	Error   string `json:"error,omitempty"` // Error message if success=false
}
