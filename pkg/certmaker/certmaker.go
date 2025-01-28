// Copyright 2024 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Package certmaker implements a certificate creation utility for Fulcio.
// It supports creating root, intermediate, and leaf certs using (AWS, GCP, Azure, HashiVault).
package certmaker

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sigstore/fulcio/pkg/ca"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/kms"
	"go.step.sm/crypto/x509util"

	// Initialize AWS KMS provider
	_ "github.com/sigstore/sigstore/pkg/signature/kms/aws"
	// Initialize Azure KMS provider
	_ "github.com/sigstore/sigstore/pkg/signature/kms/azure"
	// Initialize GCP KMS provider
	_ "github.com/sigstore/sigstore/pkg/signature/kms/gcp"
	// Initialize HashiVault KMS provider
	_ "github.com/sigstore/sigstore/pkg/signature/kms/hashivault"
)

// CryptoSignerVerifier extends SignerVerifier with CryptoSigner capability
type CryptoSignerVerifier interface {
	signature.SignerVerifier
	CryptoSigner(context.Context, func(error)) (crypto.Signer, crypto.SignerOpts, error)
}

// KMSConfig holds config for KMS providers.
type KMSConfig struct {
	CommonName        string
	Type              string
	RootKeyID         string
	IntermediateKeyID string
	LeafKeyID         string
	Options           map[string]string
}

// InitKMS initializes KMS provider based on the given config, KMSConfig.
var InitKMS = func(ctx context.Context, config KMSConfig) (signature.SignerVerifier, error) {
	if err := ValidateKMSConfig(config); err != nil {
		return nil, fmt.Errorf("invalid KMS configuration: %w", err)
	}

	var sv signature.SignerVerifier
	var err error

	switch config.Type {
	case "awskms":
		ref := fmt.Sprintf("awskms:///%s", config.RootKeyID)
		if awsRegion := config.Options["aws-region"]; awsRegion != "" {
			os.Setenv("AWS_REGION", awsRegion)
		}
		sv, err = kms.Get(ctx, ref, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize AWS KMS: %w", err)
		}

	case "gcpkms":
		ref := fmt.Sprintf("gcpkms://%s", config.RootKeyID)
		if gcpCredsFile := config.Options["gcp-credentials-file"]; gcpCredsFile != "" {
			os.Setenv("GCP_CREDENTIALS_FILE", gcpCredsFile)
		}
		sv, err = kms.Get(ctx, ref, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize GCP KMS: %w", err)
		}

	case "azurekms":
		keyURI := config.RootKeyID
		if strings.HasPrefix(config.RootKeyID, "azurekms:name=") {
			nameStart := strings.Index(config.RootKeyID, "name=") + 5
			vaultIndex := strings.Index(config.RootKeyID, ";vault=")
			if vaultIndex != -1 {
				keyName := strings.TrimSpace(config.RootKeyID[nameStart:vaultIndex])
				vaultName := strings.TrimSpace(config.RootKeyID[vaultIndex+7:])
				keyURI = fmt.Sprintf("azurekms://%s.vault.azure.net/%s", vaultName, keyName)
			}
		}
		if config.Options != nil && config.Options["azure-tenant-id"] != "" {
			azureTenantID := config.Options["azure-tenant-id"]
			os.Setenv("AZURE_TENANT_ID", azureTenantID)
			os.Setenv("AZURE_ADDITIONALLY_ALLOWED_TENANTS", "*")
		}
		os.Setenv("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com/")

		sv, err = kms.Get(ctx, keyURI, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Azure KMS: %w", err)
		}

	case "hashivault":
		keyURI := fmt.Sprintf("hashivault://%s", config.RootKeyID)
		if config.Options != nil {
			if vaultToken := config.Options["vault-token"]; vaultToken != "" {
				os.Setenv("VAULT_TOKEN", vaultToken)
			}
			if vaultAddr := config.Options["vault-address"]; vaultAddr != "" {
				os.Setenv("VAULT_ADDR", vaultAddr)
			}
		}

		sv, err = kms.Get(ctx, keyURI, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize HashiVault KMS: %w", err)
		}

	default:
		return nil, fmt.Errorf("unsupported KMS type: %s", config.Type)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get KMS signer: %w", err)
	}
	if sv == nil {
		return nil, fmt.Errorf("KMS returned nil signer")
	}

	return sv, nil
}

// CreateCertificates creates certificates using the provided KMS and templates.
// It creates 3 certificates (root -> intermediate -> leaf) if intermediateKeyID is provided,
// otherwise creates just 2 certs (root -> leaf).
func CreateCertificates(sv signature.SignerVerifier, config KMSConfig,
	rootTemplatePath, leafTemplatePath string,
	rootCertPath, leafCertPath string,
	intermediateKeyID, intermediateTemplatePath, intermediateCertPath string,
	rootLifetime, intermediateLifetime, leafLifetime time.Duration) error {

	// Create root cert
	rootPubKey, err := sv.PublicKey()
	if err != nil {
		return fmt.Errorf("error getting root public key: %w", err)
	}

	// Get crypto.Signer for root
	cryptoSV, ok := sv.(CryptoSignerVerifier)
	if !ok {
		return fmt.Errorf("signer does not implement CryptoSigner")
	}
	rootSigner, _, err := cryptoSV.CryptoSigner(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("error getting root crypto signer: %w", err)
	}

	// Use default root template if none provided
	var rootTemplate interface{}
	if rootTemplatePath == "" {
		defaultTemplate, err := GetDefaultTemplate("root")
		if err != nil {
			return fmt.Errorf("error getting default root template: %w", err)
		}
		rootTemplate = defaultTemplate
	} else {
		// Read from FS if path is provided
		content, err := os.ReadFile(rootTemplatePath)
		if err != nil {
			return fmt.Errorf("root template error: template not found at %s: %w", rootTemplatePath, err)
		}
		rootTemplate = string(content)
	}

	rootNotAfter := time.Now().UTC().Add(rootLifetime)
	rootTmpl, err := ParseTemplate(rootTemplate, nil, rootNotAfter, rootPubKey, config.CommonName)
	if err != nil {
		return fmt.Errorf("error parsing root template: %w", err)
	}

	rootTmpl.SignatureAlgorithm, err = ca.ToSignatureAlgorithm(rootSigner, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("error determining signature algorithm: %w", err)
	}

	rootCert, err := x509util.CreateCertificate(rootTmpl, rootTmpl, rootPubKey, rootSigner)
	if err != nil {
		return fmt.Errorf("error creating root certificate: %w", err)
	}

	if err := WriteCertificateToFile(rootCert, rootCertPath); err != nil {
		return fmt.Errorf("error writing root certificate: %w", err)
	}

	var signingCert *x509.Certificate
	var signingKey crypto.Signer

	if intermediateKeyID != "" {
		// Create intermediate cert if key ID is provided
		intermediateConfig := config
		intermediateConfig.RootKeyID = intermediateKeyID
		intermediateSV, err := InitKMS(context.Background(), intermediateConfig)
		if err != nil {
			return fmt.Errorf("error initializing intermediate KMS: %w", err)
		}

		intermediatePubKey, err := intermediateSV.PublicKey()
		if err != nil {
			return fmt.Errorf("error getting intermediate public key: %w", err)
		}

		intermediateCryptoSV, ok := intermediateSV.(CryptoSignerVerifier)
		if !ok {
			return fmt.Errorf("intermediate signer does not implement CryptoSigner")
		}

		intermediateSigner, _, err := intermediateCryptoSV.CryptoSigner(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("error getting intermediate crypto signer: %w", err)
		}

		var intermediateTemplate interface{}
		if intermediateTemplatePath == "" {
			defaultTemplate, err := GetDefaultTemplate("intermediate")
			if err != nil {
				return fmt.Errorf("error getting default intermediate template: %w", err)
			}
			intermediateTemplate = defaultTemplate
		} else {
			// Read from FS if path is provided
			content, err := os.ReadFile(intermediateTemplatePath)
			if err != nil {
				return fmt.Errorf("intermediate template error: template not found at %s: %w", intermediateTemplatePath, err)
			}
			intermediateTemplate = string(content)
		}

		intermediateNotAfter := time.Now().UTC().Add(intermediateLifetime)
		intermediateTmpl, err := ParseTemplate(intermediateTemplate, rootCert, intermediateNotAfter, intermediatePubKey, config.CommonName)
		if err != nil {
			return fmt.Errorf("error parsing intermediate template: %w", err)
		}

		intermediateTmpl.SignatureAlgorithm, err = ca.ToSignatureAlgorithm(intermediateSigner, crypto.SHA256)
		if err != nil {
			return fmt.Errorf("error determining signature algorithm: %w", err)
		}

		intermediateCert, err := x509util.CreateCertificate(intermediateTmpl, rootCert, intermediatePubKey, rootSigner)
		if err != nil {
			return fmt.Errorf("error creating intermediate certificate: %w", err)
		}

		if err := WriteCertificateToFile(intermediateCert, intermediateCertPath); err != nil {
			return fmt.Errorf("error writing intermediate certificate: %w", err)
		}

		signingCert = intermediateCert
		signingKey = intermediateSigner
	} else {
		signingCert = rootCert
		signingKey = rootSigner
	}

	// Create leaf cert
	leafConfig := config
	leafConfig.RootKeyID = config.LeafKeyID
	leafSV, err := InitKMS(context.Background(), leafConfig)
	if err != nil {
		return fmt.Errorf("error initializing leaf KMS: %w", err)
	}

	leafPubKey, err := leafSV.PublicKey()
	if err != nil {
		return fmt.Errorf("error getting leaf public key: %w", err)
	}

	leafCryptoSV, ok := leafSV.(CryptoSignerVerifier)
	if !ok {
		return fmt.Errorf("leaf signer does not implement CryptoSigner")
	}

	_, _, err = leafCryptoSV.CryptoSigner(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("error getting leaf crypto signer: %w", err)
	}

	var leafTemplate interface{}
	if leafTemplatePath == "" {
		defaultTemplate, err := GetDefaultTemplate("leaf")
		if err != nil {
			return fmt.Errorf("error getting default leaf template: %w", err)
		}
		leafTemplate = defaultTemplate
	} else {
		// Read from FS if path is provided
		content, err := os.ReadFile(leafTemplatePath)
		if err != nil {
			return fmt.Errorf("leaf template error: template not found at %s: %w", leafTemplatePath, err)
		}
		leafTemplate = string(content)
	}

	leafNotAfter := time.Now().UTC().Add(leafLifetime)
	leafTmpl, err := ParseTemplate(leafTemplate, signingCert, leafNotAfter, leafPubKey, config.CommonName)
	if err != nil {
		return fmt.Errorf("error parsing leaf template: %w", err)
	}

	leafTmpl.SignatureAlgorithm, err = ca.ToSignatureAlgorithm(signingKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("error determining signature algorithm: %w", err)
	}

	leafCert, err := x509util.CreateCertificate(leafTmpl, signingCert, leafPubKey, signingKey)
	if err != nil {
		return fmt.Errorf("error creating leaf certificate: %w", err)
	}

	if err := WriteCertificateToFile(leafCert, leafCertPath); err != nil {
		return fmt.Errorf("error writing leaf certificate: %w", err)
	}

	return nil
}

// Writes cert to a PEM-encoded file
func WriteCertificateToFile(cert *x509.Certificate, filename string) error {
	if cert == nil {
		return fmt.Errorf("certificate is nil")
	}
	if len(cert.Raw) == 0 {
		return fmt.Errorf("certificate has no raw data")
	}

	block := &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	}

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// get cert type
	certType := "leaf"
	if cert.IsCA {
		if cert.CheckSignatureFrom(cert) == nil {
			certType = "root"
		} else {
			certType = "intermediate"
		}
	}

	fmt.Printf("Saved %s cert to %s\n", certType, filename)
	return pem.Encode(f, block)
}

// Ensures all required KMS config params are present
func ValidateKMSConfig(config KMSConfig) error {
	if config.Type == "" {
		return fmt.Errorf("KMS type cannot be empty")
	}

	switch config.Type {
	case "awskms":
		// AWS KMS validation
		if config.Options == nil || config.Options["aws-region"] == "" {
			return fmt.Errorf("aws-region is required for AWS KMS")
		}
		validateAWSKeyID := func(keyID, keyType string) error {
			if keyID == "" {
				return nil
			}
			switch {
			case strings.HasPrefix(keyID, "arn:aws:kms:"):
				parts := strings.Split(keyID, ":")
				if len(parts) < 6 {
					return fmt.Errorf("invalid AWS KMS ARN format for %s", keyType)
				}
				if parts[3] != config.Options["aws-region"] {
					return fmt.Errorf("region in ARN (%s) does not match configured region (%s)", parts[3], config.Options["aws-region"])
				}
			case strings.HasPrefix(keyID, "alias/"):
				if strings.TrimPrefix(keyID, "alias/") == "" {
					return fmt.Errorf("alias name cannot be empty for %s", keyType)
				}
			default:
				return fmt.Errorf("awskms %s must start with 'arn:aws:kms:' or 'alias/'", keyType)
			}
			return nil
		}
		if err := validateAWSKeyID(config.RootKeyID, "RootKeyID"); err != nil {
			return err
		}
		if err := validateAWSKeyID(config.IntermediateKeyID, "IntermediateKeyID"); err != nil {
			return err
		}
		if err := validateAWSKeyID(config.LeafKeyID, "LeafKeyID"); err != nil {
			return err
		}

	case "gcpkms":
		// GCP KMS validation
		if config.Options == nil || config.Options["gcp-credentials-file"] == "" {
			return fmt.Errorf("gcp-credentials-file is required for GCP KMS")
		}
		validateGCPKeyID := func(keyID, keyType string) error {
			if keyID == "" {
				return nil
			}
			requiredComponents := []struct {
				component string
				message   string
			}{
				{"projects/", "must start with 'projects/'"},
				{"/locations/", "must contain '/locations/'"},
				{"/keyRings/", "must contain '/keyRings/'"},
				{"/cryptoKeys/", "must contain '/cryptoKeys/'"},
				{"/cryptoKeyVersions/", "must contain '/cryptoKeyVersions/'"},
			}
			for _, req := range requiredComponents {
				if !strings.Contains(keyID, req.component) {
					return fmt.Errorf("gcpkms %s %s", keyType, req.message)
				}
			}
			return nil
		}
		if err := validateGCPKeyID(config.RootKeyID, "RootKeyID"); err != nil {
			return err
		}
		if err := validateGCPKeyID(config.IntermediateKeyID, "IntermediateKeyID"); err != nil {
			return err
		}
		if err := validateGCPKeyID(config.LeafKeyID, "LeafKeyID"); err != nil {
			return err
		}

	case "azurekms":
		// Azure KMS validation
		if config.Options == nil {
			return fmt.Errorf("options map is required for Azure KMS")
		}
		if config.Options["azure-tenant-id"] == "" {
			return fmt.Errorf("azure-tenant-id is required for Azure KMS")
		}
		validateAzureKeyID := func(keyID, keyType string) error {
			if keyID == "" {
				return nil
			}
			if !strings.HasPrefix(keyID, "azurekms:name=") {
				return fmt.Errorf("azurekms %s must start with 'azurekms:name='", keyType)
			}
			nameStart := strings.Index(keyID, "name=") + 5
			vaultIndex := strings.Index(keyID, ";vault=")
			if vaultIndex == -1 {
				return fmt.Errorf("azurekms %s must contain ';vault=' parameter", keyType)
			}
			if strings.TrimSpace(keyID[nameStart:vaultIndex]) == "" {
				return fmt.Errorf("key name cannot be empty for %s", keyType)
			}
			if strings.TrimSpace(keyID[vaultIndex+7:]) == "" {
				return fmt.Errorf("vault name cannot be empty for %s", keyType)
			}
			return nil
		}
		if err := validateAzureKeyID(config.RootKeyID, "RootKeyID"); err != nil {
			return err
		}
		if err := validateAzureKeyID(config.IntermediateKeyID, "IntermediateKeyID"); err != nil {
			return err
		}
		if err := validateAzureKeyID(config.LeafKeyID, "LeafKeyID"); err != nil {
			return err
		}

	case "hashivault":
		// HashiVault KMS validation
		if config.Options == nil {
			return fmt.Errorf("options map is required for HashiVault KMS")
		}
		if config.Options["vault-token"] == "" {
			return fmt.Errorf("vault-token is required for HashiVault KMS")
		}
		if config.Options["vault-address"] == "" {
			return fmt.Errorf("vault-address is required for HashiVault KMS")
		}
		validateHashiVaultKeyID := func(keyID, keyType string) error {
			if keyID == "" {
				return nil
			}
			parts := strings.Split(keyID, "/")
			if len(parts) < 3 {
				return fmt.Errorf("hashivault %s must be in format: transit/keys/keyname", keyType)
			}
			if parts[0] != "transit" || parts[1] != "keys" {
				return fmt.Errorf("hashivault %s must start with 'transit/keys/'", keyType)
			}
			if parts[2] == "" {
				return fmt.Errorf("key name cannot be empty for %s", keyType)
			}
			return nil
		}
		if err := validateHashiVaultKeyID(config.RootKeyID, "RootKeyID"); err != nil {
			return err
		}
		if err := validateHashiVaultKeyID(config.IntermediateKeyID, "IntermediateKeyID"); err != nil {
			return err
		}
		if err := validateHashiVaultKeyID(config.LeafKeyID, "LeafKeyID"); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unsupported KMS type: %s", config.Type)
	}

	// Check that both key IDs are specified
	if config.RootKeyID == "" {
		return fmt.Errorf("RootKeyID must be specified")
	}
	if config.LeafKeyID == "" {
		return fmt.Errorf("LeafKeyID must be specified")
	}

	return nil
}
