package ghrouter

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

type certificateAuthority struct {
	certificate *x509.Certificate
	signer      crypto.Signer

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func loadCertificateAuthority(certificatePath, privateKeyPath string) (*certificateAuthority, error) {
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA private key: %w", err)
	}

	pair, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA key pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("load CA key pair: certificate is empty")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("CA certificate cannot sign certificates")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("CA private key is not a supported signing key")
	}
	if time.Now().After(certificate.NotAfter) {
		return nil, errors.New("CA certificate has expired")
	}

	return &certificateAuthority{
		certificate: certificate,
		signer:      signer,
		cache:       make(map[string]*tls.Certificate),
	}, nil
}

func (ca *certificateAuthority) certificateFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if cached := ca.cache[host]; cached != nil && cached.Leaf != nil && time.Until(cached.Leaf.NotAfter) > time.Hour {
		return cached, nil
	}
	certificate, err := ca.signHost(host)
	if err != nil {
		return nil, err
	}
	ca.cache[host] = certificate
	return certificate, nil
}

func (ca *certificateAuthority) signHost(host string) (*tls.Certificate, error) {
	now := time.Now()
	if now.After(ca.certificate.NotAfter) {
		return nil, errors.New("CA certificate has expired")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial: %w", err)
	}

	notAfter := now.Add(24 * time.Hour)
	if ca.certificate.NotAfter.Before(notAfter) {
		notAfter = ca.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, privateKey.Public(), ca.signer)
	if err != nil {
		return nil, fmt.Errorf("sign leaf certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, ca.certificate.Raw},
		PrivateKey:  privateKey,
		Leaf:        leaf,
	}, nil
}
