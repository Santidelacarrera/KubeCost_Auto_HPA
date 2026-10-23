// Package httpx builds HTTP clients for the operator's backends with optional
// TLS (custom CA, client certificate for mTLS) and bearer-token authentication.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Options configures a backend client. All fields are optional.
type Options struct {
	// CAFile is a PEM bundle used to verify the server certificate.
	CAFile string
	// CertFile/KeyFile present a client certificate (mTLS).
	CertFile, KeyFile string
	// BearerTokenFile is re-read on every request so rotated tokens are picked up.
	BearerTokenFile string
	// BearerToken is a static token (ignored when BearerTokenFile is set).
	BearerToken string
}

type bearerTransport struct {
	base  http.RoundTripper
	file  string
	token string
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token := b.token
	if b.file != "" {
		raw, err := os.ReadFile(b.file)
		if err != nil {
			return nil, fmt.Errorf("read bearer token: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		return b.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return b.base.RoundTrip(r)
}

// TLSConfig builds the client TLS configuration described by o (TLS 1.2+,
// optional custom CA and client certificate). It is shared with non-HTTP
// backends such as Redis.
func TLSConfig(o Options) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s has no valid certificates", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, fmt.Errorf("certFile and keyFile must be set together")
	}
	if o.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// NewClient returns a client with the given timeout and security options.
func NewClient(o Options, timeout time.Duration) (*http.Client, error) {
	cfg, err := TLSConfig(o)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	var rt http.RoundTripper = tr
	if o.BearerTokenFile != "" || o.BearerToken != "" {
		rt = &bearerTransport{base: tr, file: o.BearerTokenFile, token: o.BearerToken}
	}
	return &http.Client{Timeout: timeout, Transport: rt}, nil
}
