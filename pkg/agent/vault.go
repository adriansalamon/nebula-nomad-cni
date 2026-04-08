package agent

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/slackhq/nebula/cert"
)

// VaultSigner implements the Signer interface using the nebula-vault-plugin.
type VaultSigner struct {
	client       *api.Client
	mount        string
	roleID       string
	secretIDPath string
}

// NewVaultSigner creates a new Vault-backed certificate signer.
func NewVaultSigner(addr, mount, roleID, secretIDPath string) (*VaultSigner, error) {
	config := api.DefaultConfig()
	config.Address = addr

	client, err := api.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create vault client: %w", err)
	}

	vs := &VaultSigner{
		client:       client,
		mount:        strings.Trim(mount, "/"),
		roleID:       roleID,
		secretIDPath: secretIDPath,
	}

	return vs, nil
}

// SignCertificate signs a new certificate via the Vault plugin's /sign endpoint.
func (vs *VaultSigner) SignCertificate(ipStr string, roles []string, name string, duration time.Duration) (string, string, error) {
	// Ensure we have a valid token before making the request
	if err := vs.ensureValidToken(); err != nil {
		return "", "", fmt.Errorf("failed to ensure valid vault token: %w", err)
	}

	pubKey, privKey := x25519Keypair()

	pubKeyPEM := cert.MarshalPublicKeyToPEM(cert.Curve_CURVE25519, pubKey)
	data := map[string]any{
		"name":     name,
		"pub_key":  string(pubKeyPEM),
		"networks": []string{ipStr},
		"groups":   roles,
		"ttl":      duration.String(),
	}

	path := fmt.Sprintf("%s/sign", vs.mount)
	secret, err := vs.client.Logical().Write(path, data)
	if err != nil {
		return "", "", fmt.Errorf("vault sign error at %s: %w", path, err)
	}

	if secret == nil || secret.Data == nil {
		return "", "", fmt.Errorf("vault returned no data for %s", path)
	}

	signedCert, ok := secret.Data["cert"].(string)
	if !ok {
		return "", "", fmt.Errorf("vault response missing 'cert'")
	}

	privKeyPEM := cert.MarshalPrivateKeyToPEM(cert.Curve_CURVE25519, privKey)

	return signedCert, string(privKeyPEM), nil
}

// GetCACertificate retrieves the current CA certificate from Vault.
func (vs *VaultSigner) GetCACertificate() (string, error) {
	// Ensure we have a valid token before making the request
	if err := vs.ensureValidToken(); err != nil {
		return "", fmt.Errorf("failed to ensure valid vault token: %w", err)
	}

	path := fmt.Sprintf("%s/ca", vs.mount)
	secret, err := vs.client.Logical().Read(path)
	if err != nil {
		return "", fmt.Errorf("vault read error from %s: %w", path, err)
	}

	if secret == nil || secret.Data == nil {
		return "", fmt.Errorf("vault returned no data for %s", path)
	}

	caCert, ok := secret.Data["ca_cert"].(string)
	if !ok {
		return "", fmt.Errorf("vault response missing 'ca_cert'")
	}

	return caCert, nil
}

// ensureValidToken checks if the current token is valid and renews or re-authenticates if needed.
func (vs *VaultSigner) ensureValidToken() error {
	// If no AppRole credentials configured, assume token was set externally
	if vs.roleID == "" || vs.secretIDPath == "" {
		return nil
	}

	token := vs.client.Token()

	// If no token at all, authenticate
	if token == "" {
		return vs.loginAppRole(vs.roleID, vs.secretIDPath)
	}

	// Check current token TTL
	tokenInfo, err := vs.client.Auth().Token().LookupSelf()
	if err != nil {
		// Token is invalid, re-authenticate
		return vs.loginAppRole(vs.roleID, vs.secretIDPath)
	}

	// Get TTL from token info
	ttl, err := tokenInfo.TokenTTL()
	if err != nil {
		// Can't determine TTL, try to renew just in case
		return vs.renewOrReauth()
	}

	// If less than 5 minutes remaining, renew the token
	if ttl < 5*time.Minute {
		return vs.renewOrReauth()
	}

	return nil
}

// renewOrReauth attempts to renew the token, or re-authenticates if renewal fails.
func (vs *VaultSigner) renewOrReauth() error {
	_, err := vs.client.Auth().Token().RenewSelf(0)
	if err != nil {
		// Renewal failed, re-authenticate
		return vs.loginAppRole(vs.roleID, vs.secretIDPath)
	}
	return nil
}

func (vs *VaultSigner) loginAppRole(roleID, secretIDPath string) error {
	secretID, err := os.ReadFile(secretIDPath)
	if err != nil {
		return fmt.Errorf("failed to read vault secret-id: %w", err)
	}

	data := map[string]any{
		"role_id":   roleID,
		"secret_id": strings.TrimSpace(string(secretID)),
	}

	secret, err := vs.client.Logical().Write("auth/approle/login", data)
	if err != nil {
		return fmt.Errorf("vault approle login failed: %w", err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("vault login returned no auth info")
	}

	vs.client.SetToken(secret.Auth.ClientToken)
	return nil
}
