package ghrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The subprocess exits here, before the testing package can print PASS to stdout.
func TestTokenHelper(t *testing.T) {
	if os.Getenv("GH_ROUTER_TOKEN_HELPER") != "1" {
		return
	}
	args := os.Args
	index := 0
	for i, arg := range args {
		if arg == "--" {
			index = i + 1
			break
		}
	}
	if index == 0 || index >= len(args) {
		os.Exit(2)
	}
	switch args[index] {
	case "touch":
		if err := os.WriteFile(args[index+1], []byte("called"), 0o600); err != nil {
			os.Exit(6)
		}
		fmt.Print("token")
	case "print":
		fmt.Print(args[index+1])
	case "fail":
		fmt.Fprint(os.Stdout, "private-output")
		fmt.Fprint(os.Stderr, "private-error")
		os.Exit(3)
	case "large":
		fmt.Print(strings.Repeat("x", maxTokenSize+1))
	case "sleep":
		time.Sleep(time.Minute)
	case "file":
		b, err := os.ReadFile(args[index+1])
		if err != nil {
			os.Exit(4)
		}
		fmt.Print(string(b))
	case "env":
		fmt.Print(os.Getenv(args[index+1]))
	case "stdin":
		b := make([]byte, 1)
		if n, _ := os.Stdin.Read(b); n != 0 {
			os.Exit(5)
		}
		fmt.Print("no-stdin")
	}
	os.Exit(0)
}

func helperTokenSource(t *testing.T, mode string, args ...string) TokenSource {
	t.Helper()
	t.Setenv("GH_ROUTER_TOKEN_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := TokenSource{Command: &TokenCommand{Argv: append([]string{binary, "-test.run=^TestTokenHelper$", "--", mode}, args...)}}
	if err := source.validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestTokenSourceValidation(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"env":""}`, `{"file":""}`,
		`{"env":"","file":"token"}`, `{"env":"TOKEN","file":"token"}`,
		`{"file":"token","command":{"argv":["helper"]}}`,
		`{"command":{}}`, `{"command":{"argv":[""]}}`,
		`{"command":{"argv":["helper"],"cacheTTL":"0s"}}`,
		`{"command":{"argv":["helper"],"cacheTTL":"-1m"}}`,
		`{"command":{"argv":["helper"],"cacheTTL":"oops"}}`,
		`{"command":{"argv":["helper"],"timeout":"0s"}}`,
		`{"command":{"argv":["helper"],"timeout":"-1s"}}`,
		`{"command":{"argv":["helper"],"timeout":"oops"}}`,
	} {
		var source TokenSource
		if err := json.Unmarshal([]byte(input), &source); err != nil {
			t.Fatal(err)
		}
		if err := source.validate(t.TempDir()); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	source := helperTokenSource(t, "print", "token")
	if source.Command.cacheTTL != 5*time.Minute || source.Command.timeout != 30*time.Second {
		t.Fatal("unexpected command defaults")
	}
}

func TestTokenFileRefreshAndValidation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	source := TokenSource{File: testString("token")}
	if err := source.validate(directory); err != nil {
		t.Fatal(err)
	}
	p := newTokenProvider(source, "")
	for _, token := range []string{"first", "second"} {
		if err := os.WriteFile(path, []byte(" \n"+token+"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := p.get(context.Background())
		if err != nil || got != token {
			t.Fatalf("file read failed: %v", err)
		}
	}
	for _, value := range []string{"", " \n", "private token", "private\ntoken", "private\x00token", strings.Repeat("x", maxTokenSize+1)} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := p.get(context.Background()); err == nil || got != "" {
			t.Fatal("accepted invalid file token")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := p.get(context.Background()); err == nil || got != "" {
		t.Fatal("reused removed file token")
	}
}

func TestTokenCommandOutputAndFailures(t *testing.T) {
	for _, test := range []struct{ mode, value, want string }{
		{"print", " \nsecret\r\n", "secret"},
		{"stdin", "", "no-stdin"},
		{"print", "", ""},
		{"print", "private token", ""},
		{"fail", "", ""},
		{"large", "", ""},
		{"sleep", "", ""},
	} {
		t.Run(test.mode+"/"+test.want, func(t *testing.T) {
			source := helperTokenSource(t, test.mode, test.value)
			if test.mode == "sleep" {
				source.Command.timeout = 30 * time.Millisecond
			}
			got, err := source.read()
			if test.want != "" {
				if err != nil || got != test.want {
					t.Fatalf("command failed: %v", err)
				}
			} else {
				if err == nil || got != "" {
					t.Fatal("accepted invalid command result")
				}
				for _, secret := range []string{"private-output", "private-error", "private token"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("error leaked helper output")
					}
				}
			}
		})
	}
	source := helperTokenSource(t, "file", "token")
	if err := os.WriteFile(filepath.Join(source.directory, "token"), []byte("relative-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := source.read(); err != nil || got != "relative-token" {
		t.Fatalf("working directory: %v", err)
	}
	source = helperTokenSource(t, "env", "TEST_HELPER_TOKEN")
	t.Setenv("TEST_HELPER_TOKEN", "inherited-token")
	if got, err := source.read(); err != nil || got != "inherited-token" {
		t.Fatalf("environment inheritance: %v", err)
	}
	source.Command.Argv = []string{filepath.Join(t.TempDir(), "private-executable")}
	if _, err := source.read(); err == nil || strings.Contains(err.Error(), "private-executable") {
		t.Fatal("unsafe executable error")
	}
}

func TestTokenCommandExplicitShellAndRelativeExecutable(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	directory := t.TempDir()
	source := TokenSource{Command: &TokenCommand{Argv: []string{shell, "-c", "printf shell-token"}}}
	if err := source.validate(directory); err != nil {
		t.Fatal(err)
	}
	if got, err := source.read(); err != nil || got != "shell-token" {
		t.Fatalf("explicit shell: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "helper"), []byte("#!"+shell+"\nprintf relative-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	source.Command.Argv = []string{"./helper"}
	if got, err := source.read(); err != nil || got != "relative-token" {
		t.Fatalf("relative executable: %v", err)
	}
}

func TestTokenCacheRefreshFailureAndIsolation(t *testing.T) {
	source := helperTokenSource(t, "print", "first")
	p := newTokenProvider(source, "")
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	get := func(want string, fail bool) {
		t.Helper()
		got, err := p.get(context.Background())
		if (err != nil) != fail || got != want {
			t.Fatalf("unexpected cache result: %v", err)
		}
	}
	get("first", false)
	source.Command.Argv[len(source.Command.Argv)-1] = "second"
	now = now.Add(p.ttl - time.Nanosecond)
	get("first", false)
	now = now.Add(time.Nanosecond)
	get("second", false)
	other := newTokenProvider(source, "")
	source.Command.Argv[len(source.Command.Argv)-1] = "third"
	if got, err := other.get(context.Background()); err != nil || got != "third" {
		t.Fatalf("credential cache shared: %v", err)
	}
	get("second", false)
	source.Command.Argv[len(source.Command.Argv)-1] = "invalid token"
	now = now.Add(p.ttl)
	get("", true)
	source.Command.Argv[len(source.Command.Argv)-1] = "recovered"
	get("recovered", false)
}

func TestTokenCacheConcurrentWaitersAndCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	p := &tokenProvider{
		ttl: time.Minute, now: time.Now,
		read: func() (string, error) { calls.Add(1); close(started); <-release; return "token", nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := p.get(ctx); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("request cancellation not propagated")
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.get(context.Background())
			if err != nil || got != "token" {
				t.Errorf("shared result failed: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("executed %d times", calls.Load())
	}
}

func TestTokenCacheTTLStartsAfterSuccessfulRead(t *testing.T) {
	now := time.Unix(100, 0)
	p := &tokenProvider{ttl: time.Minute, now: func() time.Time { return now }}
	p.read = func() (string, error) { now = now.Add(time.Hour); return "token", nil }
	if _, err := p.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.expires.Equal(now.Add(time.Minute)) {
		t.Fatal("TTL started before read completed")
	}
}

func TestRouterDoesNotReadTokensForRejectedRequests(t *testing.T) {
	cfg := testConfig()
	router := NewRouter(cfg)
	var calls atomic.Int32
	for _, credential := range router.credentials {
		credential.provider.read = func() (string, error) { calls.Add(1); return "", errors.New("unavailable") }
	}
	for _, req := range []*http.Request{
		newRouterRequest(t, "GET", "/user", ""),
		newRouterRequest(t, "POST", "/graphql", `{"query":"query { repository(owner:\"acme\", name:\"main\"){id} repository(owner:\"partner\", name:\"other\"){id} }"}`),
	} {
		if req.URL.Path == "/user" {
			req.Header.Set("Authorization", "Bearer unknown")
		}
		if _, err := router.Select(req, apiGitHubHost); err == nil {
			t.Fatal("accepted rejected request")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("read tokens for a rejected request")
	}
	req := newRouterRequest(t, "GET", "/user", "")
	_, err := router.Select(req, apiGitHubHost)
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != 503 || calls.Load() != 1 {
		t.Fatalf("unavailable token error: %v", err)
	}
}

// Done is evaluated after get has selected the shared in-flight result.
type waitingTokenContext struct {
	context.Context
	waiting chan struct{}
}

func (c waitingTokenContext) Done() <-chan struct{} {
	close(c.waiting)
	return c.Context.Done()
}

func TestTokenCacheSharesFailureAndRetries(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	p := &tokenProvider{ttl: time.Minute, now: time.Now, read: func() (string, error) {
		if calls.Add(1) == 1 {
			<-release
			return "", errors.New("unavailable")
		}
		return "recovered", nil
	}}
	results := make(chan error, 10)
	for range 10 {
		waiting := make(chan struct{})
		go func() {
			_, err := p.get(waitingTokenContext{Context: context.Background(), waiting: waiting})
			results <- err
		}()
		<-waiting
	}
	close(release)
	for range 10 {
		if err := <-results; err == nil {
			t.Fatal("waiter did not receive the shared failure")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("failure triggered duplicate concurrent executions")
	}
	if token, err := p.get(context.Background()); err != nil || token != "recovered" || calls.Load() != 2 {
		t.Fatalf("failed acquisition was not retried: %v", err)
	}
}
