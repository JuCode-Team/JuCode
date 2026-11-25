// Package providerkit holds the upstream credential format and, under
// catalog/, the vendored provider metadata the account pool reuses instead of
// maintaining provider tables by hand.
package providerkit

import (
	"encoding/json"
	"fmt"
)

// AuthType is how an account authenticates against its provider. Only static
// API keys are supported.
type AuthType string

const AuthTypeAPIKey AuthType = "api_key"

// Credential is the decrypted credential of one upstream account. The JSON
// shape is what gets encrypted into the account row; treat it as opaque
// outside this package.
type Credential struct {
	Type   AuthType `json:"type"`
	APIKey string   `json:"api_key,omitempty"`
}

// NewAPIKeyCredential builds an api-key credential.
func NewAPIKeyCredential(key string) *Credential {
	return &Credential{Type: AuthTypeAPIKey, APIKey: key}
}

// Encode serializes the credential for at-rest encryption.
func (c *Credential) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DecodeCredential parses the stored credential JSON.
func DecodeCredential(raw string) (*Credential, error) {
	var c Credential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	return &c, nil
}

// RequestToken is the secret to present upstream for this account.
func (c *Credential) RequestToken() string {
	if c == nil {
		return ""
	}
	return c.APIKey
}
