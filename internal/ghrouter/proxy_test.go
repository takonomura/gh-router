package ghrouter

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyCONNECTReplacesAuthorization(t *testing.T) {
	captured := make(chan *http.Request, 1)
	var upstreamCalls atomic.Int32
	upstream := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		captured <- req.Clone(req.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}, "Set-Cookie": []string{"session=upstream"}},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	proxy.transport = upstream
	proxyServer := httptest.NewServer(proxy.Handler())

	bootstrapTransport := &http.Transport{Proxy: nil}
	bootstrapClient := &http.Client{Transport: bootstrapTransport}
	caResponse, err := bootstrapClient.Get(proxyServer.URL + caCertificatePath)
	if err != nil {
		t.Fatalf("download CA certificate: %v", err)
	}
	caPEM, err := io.ReadAll(caResponse.Body)
	caResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if caResponse.StatusCode != http.StatusOK {
		t.Fatalf("CA response status = %d", caResponse.StatusCode)
	}
	if caResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("CA Cache-Control = %q", caResponse.Header.Get("Cache-Control"))
	}
	if strings.Contains(string(caPEM), "PRIVATE KEY") {
		t.Fatal("CA endpoint exposed private key material")
	}
	block, rest := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("CA endpoint did not return exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !certificate.IsCA || certificate.Subject.CommonName != "gh-router ephemeral CA" {
		t.Fatalf("downloaded certificate is not the generated CA: %s", certificate.Subject)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
	}
	client := &http.Client{Transport: clientTransport}
	t.Cleanup(func() {
		bootstrapTransport.CloseIdleConnections()
		clientTransport.CloseIdleConnections()
		proxyServer.Close()
	})

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/octocat/main", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token client-secret")
	req.Header.Set("X-Forwarded-For", "attacker.example")
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if response.Header.Get("Set-Cookie") != "" {
		t.Fatal("upstream Set-Cookie was forwarded")
	}

	upstreamRequest := <-captured
	if got := upstreamRequest.Header.Get("Authorization"); got != "Bearer main-secret" {
		t.Fatalf("upstream Authorization = %q", got)
	}
	if got := upstreamRequest.Header.Get("X-Forwarded-For"); got != "" {
		t.Fatalf("upstream X-Forwarded-For = %q", got)
	}

	attackerRequest, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	attackerRequest.Header.Set("Authorization", "Bearer github_pat_attacker")
	attackerResponse, err := client.Do(attackerRequest)
	if err != nil {
		t.Fatalf("client.Do(attacker) error = %v", err)
	}
	attackerResponse.Body.Close()
	if attackerResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("attacker response status = %d", attackerResponse.StatusCode)
	}
	if attackerResponse.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("authentication rejection did not include WWW-Authenticate")
	}

	unauthenticatedRequest, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthenticatedResponse, err := client.Do(unauthenticatedRequest)
	if err != nil {
		t.Fatalf("client.Do(unauthenticated) error = %v", err)
	}
	unauthenticatedResponse.Body.Close()
	if unauthenticatedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated response status = %d", unauthenticatedResponse.StatusCode)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestProxyRejectsUnsupportedCONNECTHost(t *testing.T) {
	proxy := &Proxy{}
	req := httptest.NewRequest(http.MethodConnect, "http://evil.example:443", nil)
	req.Host = "evil.example:443"
	recorder := httptest.NewRecorder()

	proxy.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestBuildUpstreamGitRequestUsesBasicAuth(t *testing.T) {
	req := newRouterRequest(t, http.MethodPost, "/octocat/main.git/git-receive-pack", "pack")
	req.Header.Set("Authorization", "token client-secret")
	req.Header.Set("Cookie", "must-not-pass=true")

	upstream := buildUpstreamRequest(req, "github.com", "real-token")
	username, password, ok := upstream.BasicAuth()
	if !ok || username != "x-access-token" || password != "real-token" {
		t.Fatalf("BasicAuth() = %q %q %v", username, password, ok)
	}
	if upstream.Header.Get("Cookie") != "" {
		t.Fatal("Cookie was not removed")
	}
}

func TestLoadCertificateAuthority(t *testing.T) {
	testCA, roots := newTestCA(t)
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "ca.pem")
	privateKeyPath := filepath.Join(directory, "ca-key.pem")
	privateKeyDER, err := x509.MarshalECPrivateKey(testCA.signer.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testCA.certificate.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadCertificateAuthority(certificatePath, privateKeyPath)
	if err != nil {
		t.Fatalf("loadCertificateAuthority() error = %v", err)
	}
	leaf, err := loaded.certificateFor("api.github.com")
	if err != nil {
		t.Fatalf("certificateFor() error = %v", err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.github.com", Roots: roots}); err != nil {
		t.Fatalf("leaf verification failed: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestCA(t *testing.T) (*certificateAuthority, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gh-router test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &certificateAuthority{
		certificate: certificate,
		signer:      privateKey,
		cache:       make(map[string]*tls.Certificate),
	}, roots
}

func TestProxyDynamicCredentialsAndSafeFailures(t *testing.T) {
	source := helperTokenSource(t, "print", "fresh-secret")
	cfg := testConfig()
	cfg.Credentials[0].token = ""
	cfg.Credentials[0].TokenFrom = source
	var logs bytes.Buffer
	proxy, err := NewProxy(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	proxy.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Bearer fresh-secret" {
			t.Error("incorrect upstream credential")
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	request := func(host, path, authorization string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "https://"+host+path, nil)
		req.URL.Scheme, req.URL.Host = "", ""
		req.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		proxy.handleGitHubRequest(w, req, host)
		if w.Code != want {
			t.Fatalf("status = %d, want %d", w.Code, want)
		}
		for _, value := range []string{"fresh-secret", "private-output", "private-error"} {
			if strings.Contains(w.Body.String(), value) || strings.Contains(logs.String(), value) {
				t.Fatal("secret leaked in response or logs")
			}
		}
	}
	request(apiGitHubHost, "/user", "Bearer client-secret", 403)
	// A changed helper must not run after GitHub rejects the cached token.
	source.Command.Argv = helperTokenSource(t, "fail").Command.Argv
	request(apiGitHubHost, "/user", "Bearer client-secret:main", 403)
	if calls != 2 {
		t.Fatal("unexpected retry or credential fallback")
	}
	provider := proxy.router.credentials["main"].provider
	provider.mu.Lock()
	provider.expires = time.Time{}
	provider.mu.Unlock()
	request(apiGitHubHost, "/user", "Bearer client-secret", 503)
	if calls != 2 {
		t.Fatal("forwarded request after credential acquisition failure")
	}
	request(gitHubHost, "/login", "Bearer client-secret:main", 403)
}
