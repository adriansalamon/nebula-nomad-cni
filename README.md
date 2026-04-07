# Nebula CNI for Nomad

A CNI plugin and agent service that provides
[nebula](https://github.com/slackhq/nebula) overlay networking for Nomad tasks.
Could probably be adapted for use in k8s setups, but built with Nomad in mind.

## Overview

- **Automatic IP allocation** for Nomad jobs via Consul.
- **Pluggable Certificate Signing**: Support for local CA (disk-based) or **Vault Zero-Knowledge signing** using the `nebula-vault-plugin`.
- **Zero-knowledge PKI**: Only public keys are sent to Vault; private keys never leave the Nomad client node.
- **Certificate management**: Automatic issuing, signing, and **zero-downtime rotation**.
- **Distributed state**: Consul manages IP pools and allocation records across the cluster.
- **Process Isolation**: Each Nebula instance runs in a separate worker process within the task's network namespace.

## Architecture

### Components

1.  **nebula-nomad-agent** - Long-lived systemd service running on each Nomad client.
    - Manages IP allocation and certificate signing (Local or Vault-backed).
    - Orchestrates **nebula-nomad-worker** processes using systemd transient units.
    - Handles certificate rotation by pushing new configs to workers via Unix sockets.
2.  **nebula-nomad-worker** - Short-lived process managed by the agent.
    - Runs the actual Nebula instance for a single Nomad allocation.
    - Communicates with the agent for configuration reloads.
3.  **nebula-nomad-cni** - CNI plugin binary called by Nomad.
    - Communicates with the agent via Unix socket to request networking for new allocations.
4.  **Consul** - Distributed state store.
    - Stores IP pool configuration and active allocation records.

## Configuration

### Prerequisites

1.  **Nebula CA** - Either a local CA (cert/key on disk) or a Vault server with the [nebula-vault-plugin](https://github.com/adriansalamon/nebula-vault-plugin) installed.
2.  **Consul** - Cluster-wide storage for IP pools.
3.  **Config file** - Create `/etc/nebula-cni/agent.toml`. See [config.toml.example](config.toml.example).
4.  **Install Binaries** - Place `nebula-nomad-agent`, `nebula-nomad-worker`, and `nebula-nomad-cni` in appropriate paths (e.g., `/usr/local/bin/` and `/opt/cni/bin/`).

### Agent Setup

Create the systemd service for the agent (`/etc/systemd/system/nebula-nomad-agent.service`):

```ini
[Unit]
Description=Nebula Nomad Agent
After=network.target consul.service nomad.service

[Service]
Type=simple
ExecStart=/usr/local/bin/nebula-nomad-agent -config /etc/nebula-cni/agent.toml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

note: You could probably run this in other ways, e.g. as a nomad system job.

### Vault Integration (Optional)

The agent supports the
[nebula-vault-plugin](https://github.com/adriansalamon/nebula-vault-plugin),
enabling a secure **Zero-Knowledge** PKI workflow. Private keys are generated
locally on the Nomad client and never leave its memory; only public keys are
sent to Vault for signing.

Authentication is supported via either a fixed `VAULT_TOKEN` or a persistent
**AppRole** (via `role_id` and `secret_id_path`).

### Logging

The agent and CNI plugin both support structured, leveled logging via `logrus`. Verbosity can be controlled via environment variables:

- `LOG_LEVEL`: For the `nebula-nomad-agent` (default: `info`).
- `NEBULA_CNI_LOG_LEVEL`: For the `nebula-nomad-cni` binary (default: `info`).

### CNI Plugin Setup

1.  **Install CNI Binary** - Place `nebula-nomad-cni` in `/opt/cni/bin/`.
2.  **Create CNI Configuration** (`/opt/cni/config/nebula.conflist`):

```json
{
  "cniVersion": "1.0.0",
  "name": "nebula",
  "plugins": [
    {
      "type": "loopback"
    },
    {
      "type": "bridge",
      "bridge": "nomad",
      "isGateway": true,
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [
            {
              "subnet": "172.26.64.0/20"
            }
          ]
        ],
        "routes": [{ "dst": "0.0.0.0/0" }]
      }
    },
    {
      "type": "firewall",
      "backend": "iptables"
    },
    {
      "type": "nebula-nomad-cni",
      "socket_path": "/var/run/nebula-cni.sock"
    }
  ]
}
```

#### Advanced CNI Options

You can also delegate to the `macvlan` plugin to provide an additional interface (e.g., for public IP access via DHCP):

```json
{
  "type": "nebula-nomad-cni",
  "socket_path": "/var/run/nebula-cni.sock",
  "macvlan": {
    "enable": true,
    "master": "eth0",
    "name": "public0",
    "ipam": {
      "type": "dhcp"
    }
  }
}
```

3. **Configure Nomad Client** (`/etc/nomad.d/client.hcl`)

   ```hcl
   client {
     enabled = true

     cni_path = "/opt/cni/bin"
     cni_config_dir = "/opt/cni/config"
   }
   ```

## Usage

### Nomad Job Example

```hcl
job "web-app" {
  group "web" {
    network {
      mode = "cni/nebula"
    }

    service {
      name = "web-app"
      port = "80"
      address_mode = "alloc" # advertises nomad IP in consul service
    }

    task "app" {
      driver = "docker"

      meta {
        # roles to be assigned to nebula certificate
        nebula_roles = jsonencode(["web-client"])

        # Custom Nebula config, e.g. firewall rules
        nebula_config = jsonencode({
          firewall = {
            outbound = [
              {
                proto = "any"
                ports = "any"
                host = "any"
              }
            ]
            inbound = [
              {
                proto = "tcp"
                ports = "80"
                host = "any"
              }
            ]
          }
        })
      }

      config {
        image = "nginx:latest"
      }
    }
  }
}
```

## Detailed Usage

### Task Metadata

The `nebula-nomad-agent` extracts configuration from the Nomad job metadata:

- `nebula_roles`: A JSON-encoded list of roles/groups to assign to the Nebula certificate.
- `nebula_config`: A JSON-encoded object of Nebula configuration overrides (e.g., firewall rules).

### Certificate Rotation

The agent automatically handles certificate rotation before they expire.

1. It periodially checks active allocations.
2. If less than 25% of the certificate's TTL remains, it signs a new certificate.
3. It pushes the new configuration to the `nebula-nomad-worker` process via its Unix socket.
4. The worker reloads the configuration in-process, providing **zero-downtime rotation**.

### State Management & Recovery

- **Consul** is the source of truth for IP allocations.
- On startup, the agent checks Nomad for running allocations and restarts any
  missing `nebula-nomad-worker` processes.
- Stale allocation records in Consul are automaticallly cleaned up if the
  corresponding Nomad task no longer exists.

## References

- [Nebula](https://github.com/slackhq/nebula) - Overlay networking by Slack
- [Nebula CNI for Kubernetes](https://github.com/slackhq/nebula-cni) - Original inspiration
- [CNI Specification](https://github.com/containernetworking/cni/blob/main/SPEC.md)
- [Nomad CNI Documentation](https://www.nomadproject.io/docs/networking/cni)
