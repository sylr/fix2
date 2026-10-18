package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// serverTLS returns the server's TLS configuration, nil without a
// certificate.
func serverTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("--tls-cert and --tls-key go together")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// clientTLS returns the client's TLS configuration, nil without TLS. The
// server's certificate is verified against caFile if given, the system's
// roots otherwise.
func clientTLS(enabled bool, caFile, serverName string) (*tls.Config, error) {
	if !enabled {
		if caFile != "" || serverName != "" {
			return nil, errors.New("--tls-ca and --tls-server-name need --tls")
		}
		return nil, nil
	}
	cfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate", caFile)
		}
	}
	return cfg, nil
}
