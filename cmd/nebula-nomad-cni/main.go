package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	pkgVersion "github.com/adriansalamon/nebula-nomad-cni/pkg/version"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/client"
)

// NetConf is the CNI network configuration.
type NetConf struct {
	types.NetConf

	// SocketPath is the path to the agent's unix socket
	SocketPath string `json:"socket_path"`

	// RolesMetaKey is the Nomad task metadata key containing roles
	RolesMetaKey string `json:"roles_meta_key"`

	// PrevResult is the result from the previous plugin in the chain
	PrevResult *current.Result `json:"-"`

	// RawPrevResult is the raw previous result
	RawPrevResult map[string]interface{} `json:"prevResult,omitempty"`
}

type CNIArgs struct {
	types.CommonArgs

	NOMAD_ALLOC_ID   types.UnmarshallableString
	NOMAD_JOB_ID     types.UnmarshallableString
	NOMAD_GROUP_NAME types.UnmarshallableString
	NOMAD_TASK_NAME  types.UnmarshallableString
}

func main() {
	// Log to stderr (captured by systemd/journald)
	log.SetOutput(os.Stderr)

	funcs := skel.CNIFuncs{
		Add:    cmdAdd,
		Del:    cmdDel,
		Check:  cmdCheck,
		Status: cmdStatus,
	}

	skel.PluginMainFuncs(funcs, version.All, fmt.Sprintf("CNI plugin nebula-nomad-cni v%s (commit %s)", pkgVersion.Version, pkgVersion.GitCommit))
}

// cmdAdd is called when a container is created.
func cmdAdd(args *skel.CmdArgs) error {
	log.Printf("CNI ADD called: ContainerID=%s Netns=%s IfName=%s", args.ContainerID, args.Netns, args.IfName)

	// Parse network configuration
	conf, err := parseNetConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse network config: %w", err)
	}

	// Extract Nomad allocation ID from CNI_ARGS
	cniArgs := &CNIArgs{}
	if err := types.LoadArgs(args.Args, cniArgs); err != nil {
		return fmt.Errorf("failed to parse CNI_ARGS: %w", err)
	}

	allocID := cniArgs.NOMAD_ALLOC_ID

	log.Printf("CNI_ARGS: %s", args.Args)
	log.Printf("Allocation ID: %s", allocID)

	if allocID == "" || args.Netns == "" {
		return fmt.Errorf("missing required fields: alloc_id=%s netns=%s", allocID, args.Netns)
	}

	// Create client
	c := client.NewClient(conf.SocketPath)

	// Request allocation from agent (agent will query Nomad API for metadata)
	req := &client.AllocateRequest{
		AllocID: string(allocID),
		NetNS:   args.Netns,
	}

	ctx := context.Background()
	resp, err := c.Allocate(ctx, req)
	if err != nil {
		log.Printf("Failed to allocate: %v", err)
		return fmt.Errorf("failed to allocate from agent: %w", err)
	}

	if !resp.Success {
		log.Printf("Agent returned error: %s", resp.Error)
		return fmt.Errorf("agent error: %s", resp.Error)
	}

	log.Printf("Successfully allocated IP %s for allocation %s", resp.IP, cniArgs.NOMAD_ALLOC_ID)

	// Build result for Nebula interface
	ipNet := mustParseCIDR(resp.IP)
	nebulaIP := &current.IPConfig{
		Address: ipNet,
		Gateway: nil, // Nebula handles routing, no gateway needed
	}

	// Build result: preserve previous plugin results if chained, or create new result
	var result *current.Result
	if conf.PrevResult != nil {
		// Chained mode: prepend Nebula IP so it's the primary address
		// Preserve all previous result data (interfaces, routes, DNS, etc.)
		result = conf.PrevResult
		result.IPs = append([]*current.IPConfig{nebulaIP}, result.IPs...)

		// Ensure routes are preserved from bridge plugin
		// The default route should still point to eth0's gateway
		log.Printf("Preserved %d routes from previous plugins", len(result.Routes))
	} else {
		// Standalone mode: return only Nebula IP
		result = &current.Result{
			CNIVersion: conf.CNIVersion,
			IPs:        []*current.IPConfig{nebulaIP},
		}
	}

	// Return combined result
	return types.PrintResult(result, conf.CNIVersion)
}

// cmdDel is called when a container is deleted.
func cmdDel(args *skel.CmdArgs) error {
	log.Printf("CNI DEL called: ContainerID=%s Netns=%s", args.ContainerID, args.Netns)

	// Parse network configuration
	conf, err := parseNetConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse network config: %w", err)
	}

	// Extract Nomad allocation ID from CNI_ARGS
	cniArgs := &CNIArgs{}
	if err := types.LoadArgs(args.Args, cniArgs); err != nil {
		return fmt.Errorf("failed to parse CNI_ARGS: %w", err)
	}

	allocID := cniArgs.NOMAD_ALLOC_ID

	log.Printf("CNI_ARGS: %s", args.Args)
	log.Printf("Extracted NOMAD_ALLOC_ID: %s", allocID)

	if allocID == "" {
		// Fallback: try using ContainerID as allocation ID
		// In Nomad, ContainerID often equals the allocation ID
		if args.ContainerID != "" {
			log.Printf("NOMAD_ALLOC_ID not found, trying ContainerID: %s", args.ContainerID)
			allocID = types.UnmarshallableString(args.ContainerID)
		} else {
			log.Printf("Warning: No allocation ID found in CNI_ARGS and no ContainerID, cannot deallocate")
			return nil
		}
	}

	log.Printf("Deallocating allocation %s", allocID)

	// Create client
	c := client.NewClient(conf.SocketPath)

	// Request deallocation from agent
	ctx := context.Background()
	resp, err := c.Deallocate(ctx, string(allocID))
	if err != nil {
		log.Printf("Failed to deallocate: %v", err)
		return fmt.Errorf("failed to deallocate: %w", err)
	}

	if !resp.Success {
		log.Printf("Agent returned error: %s", resp.Error)
		return fmt.Errorf("agent error: %s", resp.Error)
	}

	log.Printf("Successfully deallocated allocation %s", allocID)
	return nil
}

// cmdCheck is called to verify the container's networking is as expected.
func cmdCheck(args *skel.CmdArgs) error {
	log.Printf("CNI CHECK called: ContainerID=%s", args.ContainerID)
	// For now, just return success
	return nil
}

func cmdStatus(args *skel.CmdArgs) error {
	log.Printf("CNI STATUS called: ContainerID=%s", args.ContainerID)

	// For now, just return success
	return nil
}

// parseNetConf parses the CNI network configuration.
func parseNetConf(data []byte) (*NetConf, error) {
	conf := &NetConf{
		SocketPath:   client.DefaultSocketPath,
		RolesMetaKey: "nebula_roles",
	}

	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Parse previous result if present (for plugin chaining)
	if conf.RawPrevResult != nil {
		resultBytes, err := json.Marshal(conf.RawPrevResult)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal prevResult: %w", err)
		}

		parsedResult, err := version.NewResult(conf.CNIVersion, resultBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse prevResult: %w", err)
		}

		// Convert to current version
		currentResult, err := current.NewResultFromResult(parsedResult)
		if err != nil {
			return nil, fmt.Errorf("failed to convert prevResult to current version: %w", err)
		}

		conf.PrevResult = currentResult
	}

	return conf, nil
}

// mustParseCIDR parses a CIDR string and returns an IPNet with the host IP preserved.
func mustParseCIDR(ipStr string) net.IPNet {
	// IP should have CIDR notation from agent (e.g., "10.99.0.1/24")
	if !strings.Contains(ipStr, "/") {
		// Fallback: assume /32 for IPv4
		ipStr = ipStr + "/32"
	}

	ip, ipNet, err := net.ParseCIDR(ipStr)
	if err != nil {
		panic(fmt.Sprintf("invalid IP: %s", ipStr))
	}

	// ParseCIDR returns the network address (10.99.0.0), but we want the host IP (10.99.0.1)
	// Use the IP from the first return value, with the mask from the second
	return net.IPNet{
		IP:   ip,
		Mask: ipNet.Mask,
	}
}
