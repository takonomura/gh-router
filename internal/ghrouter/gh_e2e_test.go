package ghrouter

import (
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestGHCommandRoutingE2E(t *testing.T) {
	gh := ghE2EBinary(t)

	tests := []struct {
		name           string
		repository     string
		response       string
		wantOutput     string
		wantToken      string
		wantRepository string
	}{
		{
			name:           "exact repository route",
			repository:     "acme/main",
			response:       `{"data":{"repository":{"nameWithOwner":"acme/main"}}}`,
			wantOutput:     "acme/main",
			wantToken:      "main-secret",
			wantRepository: "acme/main",
		},
		{
			name:           "owner route",
			repository:     "related/library",
			response:       `{"data":{"repository":{"nameWithOwner":"related/library"}}}`,
			wantOutput:     "related/library",
			wantToken:      "related-secret",
			wantRepository: "related/library",
		},
		{
			name:           "default route",
			repository:     "public/example",
			response:       `{"data":{"repository":{"nameWithOwner":"public/example"}}}`,
			wantOutput:     "public/example",
			wantToken:      "main-secret",
			wantRepository: "public/example",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := runGHCommand(t, gh, "gh-router-auto", test.response,
				"repo", "view", test.repository, "--json", "nameWithOwner", "--jq", ".nameWithOwner")

			if got := strings.TrimSpace(observed.output); got != test.wantOutput {
				t.Fatalf("gh output = %q, want %q", got, test.wantOutput)
			}
			if len(observed.requests) != 1 {
				t.Fatalf("upstream request count = %d, want 1", len(observed.requests))
			}
			request := observed.requests[0]
			if request.host != "api.github.com" || request.path != "/graphql" {
				t.Fatalf("upstream target = %s%s, want api.github.com/graphql", request.host, request.path)
			}
			if request.authorization != "Bearer "+test.wantToken {
				t.Fatalf("upstream Authorization = %q, want selected test token", request.authorization)
			}
			owner, repository, _ := strings.Cut(test.wantRepository, "/")
			if !strings.Contains(request.body, `"owner":"`+owner+`"`) ||
				(!strings.Contains(request.body, `"repo":"`+repository+`"`) &&
					!strings.Contains(request.body, `"name":"`+repository+`"`)) {
				t.Fatalf("GraphQL variables do not identify %q: %s", test.wantRepository, request.body)
			}
		})
	}

	t.Run("pr list uses owner route", func(t *testing.T) {
		observed := runGHCommand(t, gh, "gh-router-auto", `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`,
			"pr", "list", "--repo", "related/library", "--limit", "1", "--json", "number")
		if got := strings.TrimSpace(observed.output); got != "[]" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer related-secret")
	})

	t.Run("issue list uses exact route", func(t *testing.T) {
		observed := runGHCommand(t, gh, "gh-router-auto", `{"data":{"repository":{"hasIssuesEnabled":true,"issues":{"nodes":[]}}}}`,
			"issue", "list", "--repo", "acme/main", "--limit", "1", "--json", "number")
		if got := strings.TrimSpace(observed.output); got != "[]" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer main-secret")
	})
}

func TestGHAPICommandRoutingE2E(t *testing.T) {
	gh := ghE2EBinary(t)

	restTests := []struct {
		name       string
		repository string
		token      string
	}{
		{name: "REST exact route", repository: "acme/main", token: "main-secret"},
		{name: "REST owner route", repository: "related/library", token: "related-secret"},
		{name: "REST default route", repository: "public/example", token: "main-secret"},
	}
	for _, test := range restTests {
		t.Run(test.name, func(t *testing.T) {
			path := "/repos/" + test.repository
			observed := runGHCommand(t, gh, "gh-router-auto", `{"full_name":"`+test.repository+`"}`,
				"api", strings.TrimPrefix(path, "/"), "--jq", ".full_name")
			if got := strings.TrimSpace(observed.output); got != test.repository {
				t.Fatalf("gh output = %q", got)
			}
			assertSingleUpstreamAuthorization(t, observed.requests, path, "Bearer "+test.token)
		})
	}

	t.Run("GraphQL variables", func(t *testing.T) {
		query := `query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){nameWithOwner}}`
		observed := runGHCommand(t, gh, "gh-router-auto", `{"data":{"repository":{"nameWithOwner":"partner/project"}}}`,
			"api", "graphql", "-f", "query="+query, "-F", "owner=partner", "-F", "repo=project",
			"--jq", ".data.repository.nameWithOwner")
		if got := strings.TrimSpace(observed.output); got != "partner/project" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer partner-secret")
	})

	t.Run("GraphQL literal", func(t *testing.T) {
		query := `query { repository(owner:"related",name:"library") { nameWithOwner } }`
		observed := runGHCommand(t, gh, "gh-router-auto", `{"data":{"repository":{"nameWithOwner":"related/library"}}}`,
			"api", "graphql", "-f", "query="+query, "--jq", ".data.repository.nameWithOwner")
		if got := strings.TrimSpace(observed.output); got != "related/library" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer related-secret")
	})

	t.Run("explicit credential hint", func(t *testing.T) {
		observed := runGHCommand(t, gh, "gh-router-partner", `{"login":"octocat"}`,
			"api", "user", "--jq", ".login")
		if got := strings.TrimSpace(observed.output); got != "octocat" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/user", "Bearer partner-secret")
	})

	t.Run("unknown credential is rejected", func(t *testing.T) {
		observed := runGHCommandResult(t, gh, "attacker-controlled-token", `{"login":"must-not-arrive"}`,
			"api", "user", "--jq", ".login")
		if observed.err == nil {
			t.Fatal("gh succeeded with an unknown credential")
		}
		if len(observed.requests) != 0 {
			t.Fatalf("unknown credential reached upstream %d times", len(observed.requests))
		}
	})

	t.Run("ambiguous GraphQL route is rejected", func(t *testing.T) {
		query := `query { a: repository(owner:"related",name:"one") { id } b: repository(owner:"partner",name:"two") { id } }`
		observed := runGHCommandResult(t, gh, "gh-router-auto", `{"data":{}}`,
			"api", "graphql", "-f", "query="+query)
		if observed.err == nil {
			t.Fatal("gh succeeded with an ambiguous GraphQL route")
		}
		if len(observed.requests) != 0 {
			t.Fatalf("ambiguous request reached upstream %d times", len(observed.requests))
		}
	})
}

func TestGitSmartHTTPRoutingE2E(t *testing.T) {
	git := gitE2EBinary(t)
	advertisement := "001e# service=git-upload-pack\n00000000"

	tests := []struct {
		name       string
		repository string
		token      string
	}{
		{name: "exact repository route", repository: "acme/main", token: "main-secret"},
		{name: "owner route", repository: "related/library", token: "related-secret"},
		{name: "default route", repository: "public/example", token: "main-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runProxiedCommandResult(t, git,
				[]string{"-c", "protocol.version=0", "ls-remote", "https://github.com/" + test.repository + ".git"},
				[]string{
					"GIT_CONFIG_NOSYSTEM=1",
					"GIT_TERMINAL_PROMPT=0",
					"HOME=" + t.TempDir(),
				},
				"application/x-git-upload-pack-advertisement", advertisement)
			if result.err != nil {
				t.Fatalf("git ls-remote failed: %v\n%s\nrequests: %+v", result.err, result.output, result.requests)
			}
			if len(result.requests) != 1 {
				t.Fatalf("upstream request count = %d, want 1", len(result.requests))
			}
			request := result.requests[0]
			wantPath := "/" + test.repository + ".git/info/refs"
			if request.host != "github.com" || request.path != wantPath || request.rawQuery != "service=git-upload-pack" {
				t.Fatalf("upstream target = %s%s?%s, want github.com%s?service=git-upload-pack",
					request.host, request.path, request.rawQuery, wantPath)
			}
			wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+test.token))
			if request.authorization != wantAuthorization {
				t.Fatalf("upstream Authorization = %q, want selected test token", request.authorization)
			}
		})
	}
}

type e2eRequest struct {
	host          string
	path          string
	rawQuery      string
	authorization string
	body          string
}

type e2eResult struct {
	output   string
	requests []e2eRequest
	err      error
}

func runGHCommand(t *testing.T, gh, hint, responseBody string, args ...string) e2eResult {
	t.Helper()
	result := runGHCommandResult(t, gh, hint, responseBody, args...)
	if result.err != nil {
		t.Fatalf("gh %s failed: %v\n%s", strings.Join(args, " "), result.err, result.output)
	}
	return result
}

func runGHCommandResult(t *testing.T, gh, hint, responseBody string, args ...string) e2eResult {
	t.Helper()
	clientHome := t.TempDir()
	return runProxiedCommandResult(t, gh, args, []string{
		"GH_CONFIG_DIR=" + filepath.Join(clientHome, "config"),
		"GH_HOST=github.com",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
		"GH_TOKEN=" + hint,
		"HOME=" + clientHome,
		"XDG_STATE_HOME=" + filepath.Join(clientHome, "state"),
	}, "application/json", responseBody)
}

func runProxiedCommandResult(t *testing.T, executable string, args, environment []string, contentType, responseBody string) e2eResult {
	t.Helper()

	ca, _ := newTestCA(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certificate.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var requests []e2eRequest
	upstream := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.Body != nil {
			var err error
			body, err = io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
		}
		mu.Lock()
		requests = append(requests, e2eRequest{
			host:          req.URL.Host,
			path:          req.URL.Path,
			rawQuery:      req.URL.RawQuery,
			authorization: req.Header.Get("Authorization"),
			body:          string(body),
		})
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := newProxy(testConfig(), ca, upstream, logger, map[string]bool{"api.github.com": true, "github.com": true})
	proxyServer := httptest.NewServer(proxy.Handler())
	t.Cleanup(proxyServer.Close)

	command := exec.Command(executable, args...)
	command.Env = append(environment,
		"SSL_CERT_FILE="+caPath,
		"GIT_SSL_CAINFO="+caPath,
		"HTTPS_PROXY="+proxyServer.URL,
		"HTTP_PROXY="+proxyServer.URL,
		"NO_PROXY=",
		"https_proxy="+proxyServer.URL,
		"http_proxy="+proxyServer.URL,
		"no_proxy=")
	output, err := command.CombinedOutput()

	mu.Lock()
	defer mu.Unlock()
	return e2eResult{output: string(output), requests: append([]e2eRequest(nil), requests...), err: err}
}

func assertSingleUpstreamAuthorization(t *testing.T, requests []e2eRequest, path, authorization string) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("upstream request count = %d, want 1", len(requests))
	}
	if requests[0].host != "api.github.com" || requests[0].path != path {
		t.Fatalf("upstream target = %s%s, want api.github.com%s", requests[0].host, requests[0].path, path)
	}
	if requests[0].authorization != authorization {
		t.Fatalf("upstream Authorization = %q, want selected test token", requests[0].authorization)
	}
}

func ghE2EBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("GH_ROUTER_E2E_GH"); path != "" {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("GH_ROUTER_E2E_GH: %v", err)
		}
		return path
	}
	path, err := exec.LookPath("gh")
	if err == nil {
		return path
	}
	message := "gh was not found; install it or set GH_ROUTER_E2E_GH"
	if os.Getenv("GH_ROUTER_REQUIRE_GH_E2E") == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
	return ""
}

func gitE2EBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err == nil {
		return path
	}
	message := "git was not found"
	if os.Getenv("GH_ROUTER_REQUIRE_GH_E2E") == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
	return ""
}
