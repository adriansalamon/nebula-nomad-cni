# Nebula CNI for Nomad

A CNI plugin and agent service that provides [nebula](https://github.com/slackhq/nebula) overlay networking for Nomad tasks. Could probably be adapted for use in k8s setups, but built with Nomad in mind.

## Overview

- Automatic IP allocation for nomad jobs
- Certificate issuing, signing, and rotation
- Distributed state management via Consul

## Architecture

### Components

1. **nebula-nomad-agent** - Long-lived systemd service running on all Nomad clients
   - Manages IP allocation via Consul
   - Signs Nebula certificates
   - Runs Nebula instances as in-process goroutines (using Nebula as a Go library)
   - Exposes API via Unix socket

2. **nebula-nomad-cni** - CNI plugin binary called by Nomad
   - Communicates with agent via Unix socket
   - Returns quickly to Nomad

3. **Consul** - Distributed state storage
   - IP pool management
   - Allocation records

## Configuration

### Prerequisites

1. **Nebula CA Certificate** - Currently, all agents must share the same CA certificate.
2. **Consul** - Running and accessible from Nomad clients
3. **Config file** - See [config.toml.example](config.toml.example) for an example.
4. **Create Systemd Service** (`/etc/systemd/system/nebula-nomad-agent.service`)

```ini
[Unit]
Description=Nebula Nomad Agent
After=network.target consul.service

[Service]
Type=simple
ExecStart=/path/to/nebula-nomad-agent -config /path/to/config.toml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

note: You could probably run this in other ways, e.g. as a nomad system job.

### CNI Plugin Setup

1. **Install CNI Binary** - Put binary in `/opt/cni/bin/`
2. **Create CNI Configuration** (`/opt/cni/config/nebula.conflist`)

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
         "socket_path": "/var/run/nebula-cni.sock",
         "roles_meta_key": "nebula_roles"
       }
     ]
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
        nebula_roles = "web"

        # Custom Nebula firewall config
        nebula_config = <<EOF
firewall:
  outbound:
    - port: any
      proto: any
      host: any
  inbound:
    - proto: icmp
      port: any
      host: any
    - port: 80
      proto: tcp
      group: web-client
EOF
      }

      config {
        image = "nginx:latest"
      }
    }
  }
}
```

## References

- [Nebula](https://github.com/slackhq/nebula) - Overlay networking by Slack
- [Nebula CNI for Kubernetes](https://github.com/slackhq/nebula-cni) - Original inspiration
- [CNI Specification](https://github.com/containernetworking/cni/blob/main/SPEC.md)
- [Nomad CNI Documentation](https://www.nomadproject.io/docs/networking/cni)
