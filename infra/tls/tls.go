package tls

import (
	cryptotls "crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/webitel/webitel-go-kit/appconfig"

	"github.com/webitel/webitel-emails/config"
)

type Config struct {
	Server *cryptotls.Config
}

func New(cfg *config.Config) (*Config, error) {
	connection := cfg.Service.Connection
	if !connection.VerifyCerts {
		return &Config{}, nil
	}

	server, err := loadServer(connection.TLS)
	if err != nil {
		return nil, err
	}

	return &Config{Server: server}, nil
}

func loadServer(settings appconfig.TLS) (*cryptotls.Config, error) {
	certificate, err := cryptotls.LoadX509KeyPair(settings.Cert, settings.Key)
	if err != nil {
		return nil, fmt.Errorf("tls: load server certificate: %w", err)
	}

	caPEM, err := os.ReadFile(settings.CA)
	if err != nil {
		return nil, fmt.Errorf("tls: read client CA: %w", err)
	}

	clientCAs := x509.NewCertPool()
	if ok := clientCAs.AppendCertsFromPEM(caPEM); !ok {
		return nil, fmt.Errorf("tls: parse client CA: no certificates found")
	}

	return &cryptotls.Config{
		Certificates: []cryptotls.Certificate{certificate},
		ClientCAs:    clientCAs,
		ClientAuth:   cryptotls.RequireAndVerifyClientCert,
	}, nil
}
