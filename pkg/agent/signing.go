package agent

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"github.com/slackhq/nebula/cert"
	"golang.org/x/crypto/curve25519"
)

// CertificateSigner handles signing Nebula certificates using a CA certificate.
type CertificateSigner struct {
	caCertPath string
	caKeyPath  string
	caCert     cert.Certificate
	caKey      []byte
	caCurve    cert.Curve
}

// NewCertificateSigner creates a new certificate signer with the given CA paths.
func NewCertificateSigner(caCertPath, caKeyPath string) (*CertificateSigner, error) {
	cs := &CertificateSigner{
		caCertPath: caCertPath,
		caKeyPath:  caKeyPath,
	}

	if err := cs.loadCA(); err != nil {
		return nil, err
	}

	return cs, nil
}

// loadCA loads the CA certificate and key from disk.
func (cs *CertificateSigner) loadCA() error {
	// Load CA certificate
	caCertPEM, err := os.ReadFile(cs.caCertPath)
	if err != nil {
		return fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCert, _, err := cert.UnmarshalCertificateFromPEM(caCertPEM)
	if err != nil {
		return fmt.Errorf("failed to unmarshal CA certificate: %w", err)
	}

	if !caCert.IsCA() {
		return fmt.Errorf("certificate is not a CA certificate")
	}

	cs.caCert = caCert

	// Load CA private key
	caKeyPEM, err := os.ReadFile(cs.caKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read CA key: %w", err)
	}

	caKeyBytes, _, caCurve, err := cert.UnmarshalSigningPrivateKeyFromPEM(caKeyPEM)
	if err != nil {
		return fmt.Errorf("failed to unmarshal CA key: %w", err)
	}

	if err := caCert.VerifyPrivateKey(caCurve, caKeyBytes); err != nil {
		return fmt.Errorf("failed to verify CA key: %w", err)
	}

	cs.caKey = caKeyBytes
	cs.caCurve = caCurve

	return nil
}

// SignCertificate signs a new certificate for the given IP and roles.
// Returns the certificate and private key as PEM-encoded strings.
// ipStr should be in CIDR notation (e.g., "10.99.0.1/24")
func (cs *CertificateSigner) SignCertificate(ipStr string, roles []string, name string, duration time.Duration) (certPEM string, keyPEM string, err error) {
	// Parse IP address to netip.Prefix
	// IP should already have CIDR notation (e.g., "10.99.0.1/24")
	prefix, err := netip.ParsePrefix(ipStr)
	if err != nil {
		return "", "", fmt.Errorf("invalid IP address with CIDR: %s (%w)", ipStr, err)
	}

	// Generate new key pair for the certificate
	// Use the same curve as the CA
	pubKey, privKey, err := generateKeyPairForCurve(cs.caCurve)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate key pair: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(duration)

	switch cs.caCert.Version() {
	case cert.Version1:
		return "", "", fmt.Errorf("not implemented")
	case cert.Version2:
		// Create to-be-signed certificate
		tbs := &cert.TBSCertificate{
			Version:   cert.Version2,
			Name:      name,
			Networks:  []netip.Prefix{prefix},
			Groups:    roles,
			IsCA:      false,
			NotBefore: notBefore,
			NotAfter:  notAfter,
			PublicKey: pubKey,
			Curve:     cs.caCurve,
		}

		// Sign the certificate
		nc, err := tbs.Sign(cs.caCert, cs.caCurve, cs.caKey)
		if err != nil {
			return "", "", fmt.Errorf("failed to sign certificate: %w", err)
		}

		// Marshal certificate to PEM
		certPEMBytes, err := nc.MarshalPEM()
		if err != nil {
			return "", "", fmt.Errorf("failed to marshal certificate to PEM: %w", err)
		}

		// Marshal private key to PEM
		keyPEMBytes := cert.MarshalPrivateKeyToPEM(cs.caCurve, privKey)

		return string(certPEMBytes), string(keyPEMBytes), nil
	default:
		return "", "", fmt.Errorf("unsupported certificate version: %v", cs.caCert.Version())
	}

}

// GetCACertificate returns the CA certificate as a PEM-encoded string.
// This is useful for distributing to clients.
func (cs *CertificateSigner) GetCACertificate() (string, error) {
	caCertPEM, err := cs.caCert.MarshalPEM()
	if err != nil {
		return "", fmt.Errorf("failed to marshal CA cert to PEM: %w", err)
	}
	return string(caCertPEM), nil
}

// generateKeyPairForCurve generates a new key pair for the given curve.
func generateKeyPairForCurve(curve cert.Curve) ([]byte, []byte, error) {
	switch curve {
	case cert.Curve_CURVE25519:
		pub, rawPriv := x25519Keypair()

		return pub, rawPriv, nil
	default:
		return nil, nil, fmt.Errorf("unsupported curve: %v", curve)
	}
}

func x25519Keypair() ([]byte, []byte) {
	privkey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, privkey); err != nil {
		panic(err)
	}

	pubkey, err := curve25519.X25519(privkey, curve25519.Basepoint)
	if err != nil {
		panic(err)
	}

	return pubkey, privkey
}
