package ghrouter

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const downloadHost = "production-results-receiver.actions.githubusercontent.com"

func TestDownloadCONNECTHosts(t *testing.T) {
	proxy := &Proxy{}
	for _, test := range []struct {
		authority string
		allowed   bool
	}{
		{downloadHost + ":443", true},
		{"RAW.GITHUBUSERCONTENT.COM:443", true},
		{"githubusercontent.com:443", false},
		{".githubusercontent.com:443", false},
		{"evilgithubusercontent.com:443", false},
		{"raw.githubusercontent.com.evil.example:443", false},
		{downloadHost + ":80", false},
		{downloadHost, false},
	} {
		t.Run(test.authority, func(t *testing.T) {
			_, err := proxy.validateConnectAuthority(test.authority)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed = %v, want %v", err == nil, test.allowed)
			}
		})
	}
}

func TestDownloadRequests(t *testing.T) {
	for _, test := range []struct {
		name, method, authorization, query, cookie, host string
		duplicate, userinfo, absolute                    bool
		want                                             int
	}{
		{name: "anonymous GET", method: "GET", want: 200},
		{name: "anonymous HEAD", method: "HEAD", want: 200},
		{name: "proxy token", method: "GET", authorization: "Bearer client-secret", want: 200},
		{name: "proxy hint", method: "GET", authorization: "token client-secret:main", want: 200},
		{name: "unknown token", method: "GET", authorization: "Bearer unknown-secret", want: 401},
		{name: "unknown hint", method: "GET", authorization: "Bearer client-secret:unknown", want: 403},
		{name: "duplicate auth", method: "GET", authorization: "Bearer client-secret", duplicate: true, want: 401},
		{name: "POST", method: "POST", want: 405},
		{name: "cookie", method: "GET", cookie: "session=unknown-secret", want: 403},
		{name: "query credential", method: "GET", query: "access_token=unknown-secret", want: 403},
		{name: "client secret query", method: "GET", query: "client_secret=unknown-secret", want: 403},
		{name: "userinfo", method: "GET", userinfo: true, want: 403},
		{name: "host mismatch", method: "GET", host: "raw.githubusercontent.com", want: 400},
		{name: "host port mismatch", method: "GET", host: downloadHost + ":80", want: 400},
		{name: "absolute URL", method: "GET", absolute: true, want: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Routes = nil
			missingToken := filepath.Join(t.TempDir(), "missing-token")
			cfg.Credentials[0].TokenFrom = TokenSource{File: &missingToken}
			cfg.Credentials[0].token = ""
			var logs bytes.Buffer
			calls := 0
			proxy := newProxy(cfg, nil, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Forwarded-For"} {
					if req.Header.Get(header) != "" {
						t.Errorf("%s reached download upstream", header)
					}
				}
				if req.URL.Host != downloadHost || req.Host != downloadHost || req.URL.Scheme != "https" {
					t.Error("incorrect upstream identity")
				}
				if req.Method != test.method {
					t.Error("request method was changed")
				}
				if req.URL.RawPath != "/logs/a%2Fb" || req.URL.RawQuery != "sig=signed-secret%2B%2F&jwt=opaque+value&sig=second" {
					t.Error("signed URL was rewritten")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("log"))}, nil
			}), slog.New(slog.NewTextHandler(&logs, nil)))
			req := httptest.NewRequest(test.method, "https://"+downloadHost+"/logs/a%2Fb?sig=signed-secret%2B%2F&jwt=opaque+value&sig=second", nil)
			if !test.absolute {
				req.URL.Scheme, req.URL.Host = "", ""
			}
			if test.authorization != "" {
				req.Header.Set("Authorization", test.authorization)
			}
			if test.duplicate {
				req.Header.Add("Authorization", test.authorization)
			}
			if test.cookie != "" {
				req.Header.Set("Cookie", test.cookie)
			}
			if test.query != "" {
				req.URL.RawQuery = test.query
			}
			if test.host != "" {
				req.Host = test.host
			}
			if test.userinfo {
				req.URL.User = url.UserPassword("user", "unknown-secret")
			}
			req.Header.Set("Proxy-Authorization", "Bearer unknown-secret")
			req.Header.Set("X-Forwarded-For", "attacker.example")
			w := httptest.NewRecorder()
			proxy.handleGitHubRequest(w, req, downloadHost)
			if w.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.want, w.Body.String())
			}
			wantCalls := 0
			if test.want == 200 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls = %d, want %d", calls, wantCalls)
			}
			if test.want == 405 && w.Header().Get("Allow") != "GET, HEAD" {
				t.Error("missing Allow header")
			}
			for _, secret := range []string{"signed-secret", "unknown-secret", "client-secret", "main-secret", "/logs/a"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("log contains %q", secret)
				}
			}
		})
	}
}

func TestDownloadTunnelIdentity(t *testing.T) {
	ca, roots := newTestCA(t)
	var calls atomic.Int32
	proxy := newProxy(testConfig(), ca, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(proxy)
	defer server.Close()
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, sni, host string
		want            int
	}{
		{"matching identity", downloadHost, downloadHost, 200},
		{"mismatched Host", downloadHost, "raw.githubusercontent.com", 400},
		{"mismatched SNI", "raw.githubusercontent.com", downloadHost, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: test.sni, MinVersion: tls.VersionTLS12}}
			if test.sni != downloadHost {
				// Verify the CONNECT host's certificate so this tests the proxy's
				// SNI check rather than a client-side certificate name rejection.
				transport.TLSClientConfig.InsecureSkipVerify = true
				transport.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
					_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, DNSName: downloadHost})
					return err
				}
			}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			req, err := http.NewRequest("GET", "https://"+downloadHost+"/logs", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = test.host
			response, err := client.Do(req)
			if test.want == 0 {
				if err == nil {
					response.Body.Close()
					t.Fatal("mismatched SNI succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestDownloadUpstreamErrorDoesNotExposeURL(t *testing.T) {
	for _, test := range []struct {
		name      string
		copyError bool
		want      int
	}{
		{"transport error", false, 502},
		{"response copy error", true, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			proxy := newProxy(testConfig(), nil, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				err := &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("signed-secret")}
				if !test.copyError {
					return nil, err
				}
				reader, writer := io.Pipe()
				if closeErr := writer.CloseWithError(err); closeErr != nil {
					t.Error(closeErr)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
			}), slog.New(slog.NewTextHandler(&logs, nil)))
			req := httptest.NewRequest("GET", "https://"+downloadHost+"/logs?sig=signed-secret", nil)
			req.URL.Scheme, req.URL.Host = "", ""
			w := httptest.NewRecorder()
			proxy.handleGitHubRequest(w, req, downloadHost)
			if w.Code != test.want {
				t.Fatalf("status = %d, want %d", w.Code, test.want)
			}
			if strings.Contains(w.Body.String()+logs.String(), "signed-secret") {
				t.Fatal("signed URL leaked")
			}
		})
	}
}
