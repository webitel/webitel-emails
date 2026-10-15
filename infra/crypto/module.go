package crypto

import (
	"github.com/webitel/crypto/cryptobox"
	"github.com/webitel/crypto/cryptostore"
	"go.uber.org/fx"
)

// Module provides credential encryption backed by the shared Webitel keyring.
var Module = fx.Module(
	"crypto",
	fx.Provide(provideEncryptor),
	fx.Invoke(func(Encryptor) {}),
)

func provideEncryptor() (Encryptor, error) {
	cipher, err := cryptobox.Default()
	if err != nil {
		return nil, err
	}

	codec, err := cryptostore.NewCodec(cipher, nil)
	if err != nil {
		return nil, err
	}

	return New(codec), nil
}
