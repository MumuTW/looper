package planestrict

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

func LoadCredentials(bindingID string, keyRevision uint64, nodeID, privateKeyFile string) (Credentials, error) {
	parsedBindingID, err := parseUUID(bindingID)
	if err != nil {
		return Credentials{}, fmt.Errorf("Plane strict credentials binding ID: %w", err)
	}
	if keyRevision == 0 || nodeID == "" || privateKeyFile == "" {
		return Credentials{}, errors.New("Plane strict credentials are incomplete")
	}
	info, err := os.Stat(privateKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("stat Plane strict private key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credentials{}, errors.New("Plane strict private key must not be readable by group or others")
	}
	encoded, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("read Plane strict private key: %w", err)
	}
	block, rest := pem.Decode(encoded)
	if block == nil || len(rest) != 0 || block.Type != "PRIVATE KEY" {
		return Credentials{}, errors.New("Plane strict private key must be one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return Credentials{}, fmt.Errorf("parse Plane strict private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return Credentials{}, errors.New("Plane strict private key is not Ed25519")
	}
	return Credentials{
		BindingID: parsedBindingID, KeyRevision: keyRevision,
		PrivateKey: privateKey, NodeID: nodeID,
	}, nil
}
