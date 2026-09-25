package ghrouter

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const caCertificatePath = "/ca.pem"

const (
	apiGitHubHost = "api.github.com"
	gitHubHost    = "github.com"
)

type Proxy struct {
	router    *Router
	ca        *certificateAuthority
	transport http.RoundTripper
	logger    *slog.Logger
	sequence  atomic.Uint64
}

func NewProxy(cfg *Config, logger *slog.Logger) (*Proxy, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var ca *certificateAuthority
	var err error
	if cfg.Server.CA == nil {
		ca, err = generateCertificateAuthority()
		if err == nil {
			logger.Info("ephemeral CA generated", "certificate_path", caCertificatePath, "expires", ca.certificate.NotAfter)
		}
	} else {
		ca, err = loadCertificateAuthority(cfg.Server.CA.CertificateFile, cfg.Server.CA.PrivateKeyFile)
	}
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return newProxy(cfg, ca, transport, logger), nil
}

func newProxy(cfg *Config, ca *certificateAuthority, transport http.RoundTripper, logger *slog.Logger) *Proxy {
	return &Proxy{
		router:    NewRouter(cfg),
		ca:        ca,
		transport: transport,
		logger:    logger,
	}
}

func (p *Proxy) Handler() http.Handler {
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	requestID := p.nextRequestID()
	if req.URL.Path == caCertificatePath && req.URL.RawQuery == "" {
		if req.Method != http.MethodGet {
			p.writeError(w, requestID, http.StatusMethodNotAllowed, "get_required")
			return
		}
		p.serveCACertificate(w, requestID)
		return
	}
	if req.Method != http.MethodConnect {
		p.writeError(w, requestID, http.StatusMethodNotAllowed, "connect_required")
		return
	}

	host, err := p.validateConnectAuthority(req.Host)
	if err != nil {
		requestErr := asRequestError(err)
		p.writeError(w, requestID, requestErr.Status, requestErr.Code)
		return
	}
	certificate, err := p.ca.certificateFor(host)
	if err != nil {
		p.logger.Error("certificate generation failed", "request_id", requestID, "host", host, "error", err)
		p.writeError(w, requestID, http.StatusInternalServerError, "certificate_unavailable")
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.writeError(w, requestID, http.StatusInternalServerError, "hijacking_unavailable")
		return
	}
	clientConn, buffered, err := hijacker.Hijack()
	if err != nil {
		p.logger.Warn("CONNECT hijack failed", "request_id", requestID, "host", host, "error", err)
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		clientConn.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		clientConn.Close()
		return
	}
	if buffered.Reader.Buffered() != 0 {
		clientConn = &readerConn{Conn: clientConn, reader: buffered.Reader}
	}

	p.serveTunnel(clientConn, host, certificate, requestID)
}

func (p *Proxy) serveCACertificate(w http.ResponseWriter, requestID string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="gh-router-ca.pem"`)
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Gh-Router-Request-Id", requestID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.ca.certificatePEM())
}

func (p *Proxy) validateConnectAuthority(authority string) (string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port != "443" {
		return "", newRequestError(http.StatusForbidden, "host_denied", errors.New("CONNECT requires an allowed host on port 443"))
	}
	host = strings.ToLower(host)
	if !isSupportedGitHubHost(host) {
		return "", newRequestError(http.StatusForbidden, "host_denied", nil)
	}
	return host, nil
}

func isSupportedGitHubHost(host string) bool {
	return host == apiGitHubHost || host == gitHubHost
}

func (p *Proxy) serveTunnel(clientConn net.Conn, host string, certificate *tls.Certificate, connectRequestID string) {
	defer clientConn.Close()
	if err := clientConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*certificate},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		p.logger.Warn("client TLS handshake failed", "request_id", connectRequestID, "host", host, "error", err)
		return
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		return
	}
	serverName := strings.ToLower(tlsConn.ConnectionState().ServerName)
	if serverName == "" || serverName != host {
		p.logger.Warn("client SNI mismatch", "request_id", connectRequestID, "host", host, "sni", serverName)
		return
	}

	tracked := newTrackedConn(tlsConn)
	listener := &singleConnListener{conn: tracked}
	innerServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p.handleGitHubRequest(w, req, host)
		}),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          discardHTTPErrorLog(),
	}
	err := innerServer.Serve(listener)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		p.logger.Debug("tunnel closed", "request_id", connectRequestID, "host", host, "error", err)
	}
}

func (p *Proxy) handleGitHubRequest(w http.ResponseWriter, req *http.Request, connectHost string) {
	requestID := p.nextRequestID()
	started := time.Now()

	requestHost, err := canonicalRequestHost(req.Host)
	if err != nil || requestHost != connectHost || req.URL.IsAbs() {
		p.writeError(w, requestID, http.StatusBadRequest, "host_mismatch")
		return
	}
	selection, err := p.router.Select(req, connectHost)
	if err != nil {
		requestErr := asRequestError(err)
		p.logger.Warn("request rejected",
			"request_id", requestID,
			"host", connectHost,
			"method", req.Method,
			"code", requestErr.Code,
		)
		if requestErr.Status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="gh-router"`)
		}
		p.writeError(w, requestID, requestErr.Status, requestErr.Code)
		return
	}

	upstreamRequest := buildUpstreamRequest(req, connectHost, selection.Token)
	response, err := p.transport.RoundTrip(upstreamRequest)
	if err != nil {
		p.logger.Warn("upstream request failed",
			"request_id", requestID,
			"host", connectHost,
			"method", req.Method,
			"target", selection.Target,
			"credential", selection.CredentialID,
			"error_type", fmt.Sprintf("%T", err),
		)
		p.writeError(w, requestID, http.StatusBadGateway, "upstream_error")
		return
	}
	defer response.Body.Close()

	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("X-Gh-Router-Request-Id", requestID)
	w.WriteHeader(response.StatusCode)
	_, copyErr := io.Copy(w, response.Body)

	attributes := []any{
		"request_id", requestID,
		"host", connectHost,
		"method", req.Method,
		"target", selection.Target,
		"credential", selection.CredentialID,
		"status", response.StatusCode,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	if copyErr != nil {
		attributes = append(attributes, "error", copyErr)
		p.logger.Warn("request completed with response copy error", attributes...)
		return
	}
	p.logger.Info("request completed", attributes...)
}

func buildUpstreamRequest(req *http.Request, host, token string) *http.Request {
	upstreamRequest := req.Clone(req.Context())
	clonedURL := *req.URL
	clonedURL.Scheme = "https"
	clonedURL.Host = host
	upstreamRequest.URL = &clonedURL
	upstreamRequest.RequestURI = ""
	upstreamRequest.Host = host

	removeHopHeaders(upstreamRequest.Header)
	for _, header := range []string{
		"Authorization",
		"Cookie",
		"Forwarded",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Via",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
	} {
		upstreamRequest.Header.Del(header)
	}
	if host == apiGitHubHost {
		upstreamRequest.Header.Set("Authorization", "Bearer "+token)
	} else {
		upstreamRequest.SetBasicAuth("x-access-token", token)
	}
	return upstreamRequest
}

func copyResponseHeaders(destination, source http.Header) {
	cloned := source.Clone()
	removeHopHeaders(cloned)
	cloned.Del("Proxy-Authenticate")
	cloned.Del("Set-Cookie")
	for key, values := range cloned {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func removeHopHeaders(header http.Header) {
	for _, connection := range header.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(name)
	}
}

func canonicalRequestHost(authority string) (string, error) {
	if host, port, err := net.SplitHostPort(authority); err == nil {
		if port != "443" {
			return "", errors.New("unexpected port")
		}
		return strings.ToLower(host), nil
	}
	if strings.Contains(authority, ":") || authority == "" {
		return "", errors.New("invalid host")
	}
	return strings.ToLower(authority), nil
}

func (p *Proxy) nextRequestID() string {
	return fmt.Sprintf("ghr-%x", p.sequence.Add(1))
}

func (p *Proxy) writeError(w http.ResponseWriter, requestID string, status int, code string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Gh-Router-Request-Id", requestID)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "gh-router: %s\n", code)
}

func asRequestError(err error) *RequestError {
	var requestErr *RequestError
	if errors.As(err, &requestErr) {
		return requestErr
	}
	return newRequestError(http.StatusBadRequest, "malformed_request", err)
}

type readerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type trackedConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func newTrackedConn(conn net.Conn) *trackedConn {
	return &trackedConn{Conn: conn, done: make(chan struct{})}
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

type singleConnListener struct {
	conn     *trackedConn
	mu       sync.Mutex
	accepted bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.conn.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	return l.conn.Close()
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

func discardHTTPErrorLog() *log.Logger {
	return log.New(io.Discard, "", 0)
}
