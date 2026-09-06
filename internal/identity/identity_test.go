package identity

import (
	"crypto/ed25519"
	"testing"
)

func TestGenerateAndLoadDevice(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatalf("generate device: %v", err)
	}

	pubToken := dev.EncodedPublicKey()
	decodedPub, err := DecodeToken(pubToken, ed25519.PublicKeySize)
	if err != nil {
		t.Fatalf("decode pub token: %v", err)
	}
	if string(decodedPub) != string(dev.PublicKey) {
		t.Fatal("decoded public key does not match")
	}

	loaded, err := LoadDevice(dev.PrivateKey, dev.CertDER)
	if err != nil {
		t.Fatalf("load device: %v", err)
	}
	if loaded.EncodedPublicKey() != pubToken {
		t.Fatal("loaded public key does not match")
	}

	tlsCert, err := dev.TLSCertificate()
	if err != nil || len(tlsCert.Certificate) == 0 {
		t.Fatalf("tls certificate: %v", err)
	}
}
