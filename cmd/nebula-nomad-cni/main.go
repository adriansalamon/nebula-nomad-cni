package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	pkgVersion "github.com/adriansalamon/nebula-nomad-cni/pkg/version"

	"github.com/adriansalamon/nebula-nomad-cni/pkg/client"
)

// MacVLANConfig holds optional macvlan delegation configuration
type MacVLANConfig struct {
	Enable   bool           `json:"enable"`
	Master   string         `json:"master"`
	Name     string         `json:"name"`
	Firewall bool           `json:"firewall"`
	IPAM     map[string]any `json:"ipam"`
}

// NetConf is the CNI network configuration.
type NetConf struct {
	types.NetConf

	// SocketPath is the path to the agent's unix socket
	SocketPath string `json:"socket_path"`
	// RolesMetaKey is the Nomad task metadata key containing roles
	RolesMetaKey string `json:"roles_meta_key"`

	// MacVLAN holds optional macvlan delegation configuration
	MacVLAN *MacVLANConfig `json:"macvlan,omitempty"`

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
	logrus.SetOutput(os.Stderr)
	// Set level from env or default to Info
	level, _ := logrus.ParseLevel(os.Getenv("NEBULA_CNI_LOG_LEVEL"))
	if level == 0 {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		DisableTimestamp: true, // systemd/nomad already adds timestamps
	})

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

	if allocID == "" || args.Netns == "" {
		return fmt.Errorf("missing required fields: alloc_id=%s netns=%s", allocID, args.Netns)
	}

	// Build result: preserve previous plugin results if chained, or create new result
	var result *current.Result
	if conf.PrevResult != nil {
		// Chained mode: preserve all previous result data
		result = conf.PrevResult
	} else {
		// Standalone mode: return new result
		result = &current.Result{
			CNIVersion: conf.CNIVersion,
			IPs:        []*current.IPConfig{},
		}
	}

	// Delegate to macvlan plugin if configured
	if conf.MacVLAN != nil && conf.MacVLAN.Enable {
		macvlanResult, err := delegateMacvlan(args, conf)
		if err != nil {
			return fmt.Errorf("failed to delegate to macvlan: %w", err)
		}

		// Merge macvlan result with our result
		result.Interfaces = append(result.Interfaces, macvlanResult.Interfaces...)
		result.IPs = append(result.IPs, macvlanResult.IPs...)
		result.Routes = append(result.Routes, macvlanResult.Routes...)

		// Get the network namespace and applying firewall rules specifically for the macvlan interface
		if conf.MacVLAN.Firewall {
			netNS, err := ns.GetNS(args.Netns)
			if err != nil {
				return fmt.Errorf("failed to open netns %q: %w", args.Netns, err)
			}

			if err := netNS.Do(func(_ ns.NetNS) error {
				return applyFirewall(conf.MacVLAN.Name)
			}); err != nil {
				netNS.Close()
				return fmt.Errorf("failed to apply firewall: %w", err)
			}
			netNS.Close()
		}
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
		logrus.Errorf("Failed to allocate: %v", err)
		return fmt.Errorf("failed to allocate from agent: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("agent error: %s", resp.Error)
	}

	// Build result for Nebula interface
	ipNet := mustParseCIDR(resp.IP)
	nebulaIP := &current.IPConfig{
		Address: ipNet,
		Gateway: nil, // Nebula handles routing, no gateway needed
	}

	// Prefix Nebula IP
	result.IPs = append([]*current.IPConfig{nebulaIP}, result.IPs...)

	// Return combined result
	return types.PrintResult(result, conf.CNIVersion)
}

// delegateMacvlan delegates to the macvlan CNI plugin with DHCP IPAM
func delegateMacvlan(args *skel.CmdArgs, conf *NetConf) (*current.Result, error) {
	if os.Setenv("CNI_IFNAME", conf.MacVLAN.Name) != nil {
		return nil, fmt.Errorf("failed to set CNI_IFNAME")
	}

	// Build macvlan plugin configuration
	macvlanConf := map[string]interface{}{
		"cniVersion": conf.CNIVersion,
		"type":       "macvlan",
		"name":       conf.MacVLAN.Name,
		"master":     conf.MacVLAN.Master,
		"ipam":       conf.MacVLAN.IPAM,
	}

	// Marshal to JSON
	macvlanConfBytes, err := json.Marshal(macvlanConf)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal macvlan config: %w", err)
	}

	origIfName := args.IfName
	args.IfName = conf.MacVLAN.Name

	result, err := invoke.DelegateAdd(context.TODO(), "macvlan", macvlanConfBytes, nil)
	args.IfName = origIfName

	if err != nil {
		return nil, fmt.Errorf("macvlan plugin failed: %w", err)
	}

	// Convert to current version
	currentResult, err := current.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("failed to convert macvlan result: %w", err)
	}

	return currentResult, nil
}

// delegateMacvlanDel delegates cleanup to the macvlan CNI plugin
func delegateMacvlanDel(args *skel.CmdArgs, conf *NetConf) error {
	if os.Setenv("CNI_IFNAME", conf.MacVLAN.Name) != nil {
		return fmt.Errorf("failed to set CNI_IFNAME")
	}
	// Build macvlan plugin configuration (same as ADD)
	macvlanConf := map[string]interface{}{
		"cniVersion": conf.CNIVersion,
		"type":       "macvlan",
		"name":       conf.MacVLAN.Name,
		"master":     conf.MacVLAN.Master,
		"ipam":       conf.MacVLAN.IPAM,
	}

	// Marshal to JSON
	macvlanConfBytes, err := json.Marshal(macvlanConf)
	if err != nil {
		return fmt.Errorf("failed to marshal macvlan config: %w", err)
	}

	origIfName := args.IfName
	args.IfName = conf.MacVLAN.Name

	err = invoke.DelegateDel(context.Background(), "macvlan", macvlanConfBytes, nil)
	args.IfName = origIfName

	if err != nil {
		return fmt.Errorf("macvlan plugin cleanup failed: %w", err)
	}

	return nil
}

// cmdDel is called when a container is deleted.
func cmdDel(args *skel.CmdArgs) error {

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

	if allocID == "" {
		// Fallback: try using ContainerID as allocation ID
		// In Nomad, ContainerID often equals the allocation ID
		if args.ContainerID != "" {
			allocID = types.UnmarshallableString(args.ContainerID)
		} else {
			return nil
		}
	}

	// Create client
	c := client.NewClient(conf.SocketPath)

	// Request deallocation from agent
	ctx := context.Background()
	resp, err := c.Deallocate(ctx, string(allocID))
	if err != nil {
		logrus.Errorf("Failed to deallocate: %v", err)
		return fmt.Errorf("failed to deallocate: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("agent error: %s", resp.Error)
	}

	// Delegate to macvlan plugin for cleanup if configured
	if conf.MacVLAN != nil && conf.MacVLAN.Enable {

		if err := delegateMacvlanDel(args, conf); err != nil {
			// Log but don't fail - cleanup is best-effort
			logrus.Warnf("failed to delegate macvlan cleanup: %v", err)
		}
	}

	return nil
}

// cmdCheck is called to verify the container's networking is as expected.
func cmdCheck(args *skel.CmdArgs) error {
	// For now, just return success
	return nil
}

func cmdStatus(args *skel.CmdArgs) error {
	// For now, just return success
	return nil
}

// parseNetConf parses the CNI network configuration.
func parseNetConf(data []byte) (*NetConf, error) {
	conf := &NetConf{
		SocketPath: client.DefaultSocketPath,
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

func applyFirewall(macvlanIfName string) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("failed to init nftables netlink: %w", err)
	}

	// Create table cni_filter
	tb := c.AddTable(&nftables.Table{
		Family: nftables.TableFamilyINet,
		Name:   "cni_filter",
	})

	// Create input chain
	policyAccept := nftables.ChainPolicyAccept
	ch := c.AddChain(&nftables.Chain{
		Name:     "input",
		Table:    tb,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyAccept,
	})

	macvlanBytes := make([]byte, 16)
	copy(macvlanBytes, macvlanIfName)

	// Rule 1: Accept return traffic for outbound connections on the macvlan interface
	// iifname "$macvlanIfName" ct state established,related accept
	c.AddRule(&nftables.Rule{
		Table: tb,
		Chain: ch,
		Exprs: []expr.Any{
			// Match iifname == macvlanIfName
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     macvlanBytes,
			},
			// Load ct state -> register 1
			&expr.Ct{
				Register:       1,
				SourceRegister: false,
				Key:            expr.CtKeySTATE,
			},
			// Bitwise AND with 0x06 (ESTABLISHED | RELATED)
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           []byte{0x06, 0x00, 0x00, 0x00},
				Xor:            []byte{0x00, 0x00, 0x00, 0x00},
			},
			// Check if != 0
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     []byte{0x00, 0x00, 0x00, 0x00},
			},
			// Verdict: ACCEPT
			&expr.Verdict{
				Kind: expr.VerdictAccept,
			},
		},
	})

	// Rule 2: Drop all other unsolicited inbound traffic from the macvlan interface
	// iifname "$macvlanIfName" drop
	c.AddRule(&nftables.Rule{
		Table: tb,
		Chain: ch,
		Exprs: []expr.Any{
			// Match iifname == macvlanIfName
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     macvlanBytes,
			},
			// Verdict: DROP
			&expr.Verdict{
				Kind: expr.VerdictDrop,
			},
		},
	})

	// Execute transaction
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables flush failed: %w", err)
	}

	return nil
}
