// Package crypto provides application-level encryption for stored credentials.
package crypto

import "context"

// Encryptor seals and opens sensitive values without exposing the backing
// cryptographic implementation to the service layer.
type Encryptor interface {
	Encrypt(ctx context.Context, plain []byte) ([]byte, error)
	Decrypt(ctx context.Context, blob []byte) ([]byte, error)
}
