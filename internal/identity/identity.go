package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"
)

type Device struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	CertDER    []byte
}

func GenerateDevice() (*Device, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate device key: %w", err)
	}

	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	serial := new(big.Int).SetBytes(serialBytes)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "woolwire-device"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"woolwire-device"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create device certificate: %w", err)
	}

	return &Device{
		PrivateKey: priv,
		PublicKey:  pub,
		CertDER:    certDER,
	}, nil
}

func LoadDevice(privKey []byte, certDER []byte) (*Device, error) {
	if len(privKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private key size")
	}
	priv := ed25519.PrivateKey(privKey)
	pub := priv.Public().(ed25519.PublicKey)
	return &Device{
		PrivateKey: priv,
		PublicKey:  pub,
		CertDER:    certDER,
	}, nil
}

func (d *Device) EncodedPublicKey() string {
	return EncodeToken(d.PublicKey)
}

func (d *Device) TLSCertificate() (tls.Certificate, error) {
	if len(d.CertDER) == 0 || len(d.PrivateKey) == 0 {
		return tls.Certificate{}, errors.New("device credentials incomplete")
	}
	return tls.Certificate{
		Certificate: [][]byte{d.CertDER},
		PrivateKey:  d.PrivateKey,
	}, nil
}

func EncodeToken(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeToken(s string, expectedLen int) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if expectedLen > 0 && len(b) != expectedLen {
		return nil, fmt.Errorf("expected token length %d, got %d", expectedLen, len(b))
	}
	return b, nil
}
