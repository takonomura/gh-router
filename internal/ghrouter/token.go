package ghrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxTokenSize = 64 << 10

// TokenSource selects exactly one way to obtain a token.
type TokenSource struct {
	Env       *string       `json:"env,omitempty"`
	File      *string       `json:"file,omitempty"`
	Command   *TokenCommand `json:"command,omitempty"`
	directory string
}

type TokenCommand struct {
	Argv     []string `json:"argv"`
	CacheTTL string   `json:"cacheTTL,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
	cacheTTL time.Duration
	timeout  time.Duration
}

func (s *TokenSource) validate(directory string) error {
	count := 0
	if s.Env != nil {
		count++
	}
	if s.File != nil {
		count++
	}
	if s.Command != nil {
		count++
	}
	if count != 1 {
		return errors.New("must specify exactly one of env, file, or command")
	}
	s.directory = directory
	if s.Env != nil && (strings.TrimSpace(*s.Env) == "" || strings.ContainsAny(*s.Env, "=\x00")) {
		return errors.New("env must name an environment variable")
	}
	if s.File != nil {
		if strings.TrimSpace(*s.File) == "" || strings.ContainsRune(*s.File, 0) {
			return errors.New("file must specify a path")
		}
		path := resolveConfigPath(directory, *s.File)
		s.File = &path
	}
	if c := s.Command; c != nil {
		if len(c.Argv) == 0 || strings.TrimSpace(c.Argv[0]) == "" {
			return errors.New("command.argv must specify an executable")
		}
		for _, arg := range c.Argv {
			if strings.ContainsRune(arg, 0) {
				return errors.New("command.argv must not contain NUL")
			}
		}
		var err error
		c.cacheTTL, err = tokenDuration(c.CacheTTL, 5*time.Minute)
		if err != nil {
			return errors.New("command.cacheTTL must be a positive duration")
		}
		c.timeout, err = tokenDuration(c.Timeout, 30*time.Second)
		if err != nil {
			return errors.New("command.timeout must be a positive duration")
		}
	}
	return nil
}

func tokenDuration(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New("invalid duration")
	}
	return duration, nil
}

// read returns only sanitized errors; filesystem and command errors can contain secrets.
func (s TokenSource) read() (string, error) {
	switch {
	case s.Env != nil:
		token, ok := os.LookupEnv(*s.Env)
		if !ok || token == "" {
			return "", errors.New("environment token is empty or unset")
		}
		if token != strings.TrimSpace(token) {
			return "", errors.New("environment token contains surrounding whitespace")
		}
		return validateToken(token)
	case s.File != nil:
		f, err := os.Open(*s.File)
		if err != nil {
			return "", errors.New("cannot open token file")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxTokenSize+1))
		if err != nil {
			return "", errors.New("cannot read token file")
		}
		return parseToken(data)
	case s.Command != nil:
		return s.runCommand()
	default:
		return "", errors.New("token source is missing")
	}
}

func (s TokenSource) runCommand() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.Command.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Command.Argv[0], s.Command.Argv[1:]...)
	cmd.Dir = s.directory
	// Bound pipe draining even if a descendant retains stdout after the helper exits.
	cmd.WaitDelay = 100 * time.Millisecond
	configureTokenCommand(cmd)
	output := &tokenOutput{}
	cmd.Stdout = output
	// nil Stdin/Stderr use the null device and never expose helper diagnostics.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("token command timed out")
		}
		if output.exceeded {
			return "", errors.New("token command output exceeds 64 KiB")
		}
		return "", errors.New("token command failed")
	}
	return parseToken(output.buffer.Bytes())
}

type tokenOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (w *tokenOutput) Write(p []byte) (int, error) {
	if len(p) > maxTokenSize-w.buffer.Len() {
		w.exceeded = true
		return 0, errors.New("token output too large")
	}
	return w.buffer.Write(p)
}

func parseToken(data []byte) (string, error) {
	if len(data) > maxTokenSize {
		return "", errors.New("token exceeds 64 KiB")
	}
	return validateToken(strings.TrimSpace(string(data)))
}

func validateToken(token string) (string, error) {
	if token == "" || len(token) > maxTokenSize || !utf8.ValidString(token) {
		return "", errors.New("token is empty, too large, or invalid")
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("token contains whitespace or control characters")
		}
	}
	return token, nil
}

// A provider belongs to one runtime credential. Only command results are cached.
type tokenProvider struct {
	read    func() (string, error)
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	token   string
	expires time.Time
	pending *tokenResult
}

type tokenResult struct {
	done  chan struct{}
	token string
	err   error
}

func newTokenProvider(source TokenSource, initial string) *tokenProvider {
	p := &tokenProvider{read: source.read, now: time.Now}
	if source.Command != nil {
		p.ttl = source.Command.cacheTTL
	} else if initial != "" {
		p.read = func() (string, error) { return initial, nil }
	}
	return p
}

func (p *tokenProvider) get(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.ttl == 0 {
		return p.read()
	}
	p.mu.Lock()
	if p.token != "" && p.now().Before(p.expires) {
		token := p.token
		p.mu.Unlock()
		return token, nil
	}
	result := p.pending
	if result == nil {
		result = &tokenResult{done: make(chan struct{})}
		p.pending = result
		go p.refresh(result)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-result.done:
		return result.token, result.err
	}
}

func (p *tokenProvider) refresh(result *tokenResult) {
	token, err := p.read()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.token = token
		p.expires = p.now().Add(p.ttl)
	} else {
		p.token = ""
		p.expires = time.Time{}
	}
	result.token, result.err = token, err
	p.pending = nil
	close(result.done)
}

func tokenSourceError(field string, err error) error {
	return fmt.Errorf("config: %s: %w", field, err)
}
