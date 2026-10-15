package crypto

import (
	"context"
	"errors"

	"github.com/webitel/crypto/cryptostore"
)

// ErrNotCiphertext reports a stored credential that was not produced by the
// configured Webitel encryptor.
var ErrNotCiphertext = errors.New("crypto: stored value is not ciphertext")

type codecEncryptor struct {
	codec *cryptostore.Codec
}

// New creates an Encryptor backed by the shared Webitel credential codec.
func New(codec *cryptostore.Codec) Encryptor {
	return codecEncryptor{codec: codec}
}

func (e codecEncryptor) Encrypt(ctx context.Context, plain []byte) ([]byte, error) {
	return e.codec.Encrypt(ctx, plain)
}

func (e codecEncryptor) Decrypt(ctx context.Context, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, nil
	}

	if inner, ok := cryptostore.UnframeBinary(blob); ok && len(inner) == 0 {
		return nil, ErrNotCiphertext
	}

	data, encrypted, err := e.codec.Decrypt(ctx, blob)
	if err != nil {
		return nil, err
	}
	if !encrypted {
		return nil, ErrNotCiphertext
	}

	return data, nil
}
