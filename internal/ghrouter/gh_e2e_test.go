package ghrouter

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
			name:           "unconditional route",
			repository:     "public/example",
			response:       `{"data":{"repository":{"nameWithOwner":"public/example"}}}`,
			wantOutput:     "public/example",
			wantToken:      "main-secret",
			wantRepository: "public/example",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := runGHCommand(t, gh, "client-secret", test.response,
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
		observed := runGHCommand(t, gh, "client-secret", `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`,
			"pr", "list", "--repo", "related/library", "--limit", "1", "--json", "number")
		if got := strings.TrimSpace(observed.output); got != "[]" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer related-secret")
	})

	t.Run("issue list uses exact route", func(t *testing.T) {
		observed := runGHCommand(t, gh, "client-secret", `{"data":{"repository":{"hasIssuesEnabled":true,"issues":{"nodes":[]}}}}`,
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
		{name: "REST unconditional route", repository: "public/example", token: "main-secret"},
	}
	for _, test := range restTests {
		t.Run(test.name, func(t *testing.T) {
			path := "/repos/" + test.repository
			observed := runGHCommand(t, gh, "client-secret", `{"full_name":"`+test.repository+`"}`,
				"api", strings.TrimPrefix(path, "/"), "--jq", ".full_name")
			if got := strings.TrimSpace(observed.output); got != test.repository {
				t.Fatalf("gh output = %q", got)
			}
			assertSingleUpstreamAuthorization(t, observed.requests, path, "Bearer "+test.token)
		})
	}

	t.Run("GraphQL variables", func(t *testing.T) {
		query := `query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){nameWithOwner}}`
		observed := runGHCommand(t, gh, "client-secret", `{"data":{"repository":{"nameWithOwner":"partner/project"}}}`,
			"api", "graphql", "-f", "query="+query, "-F", "owner=partner", "-F", "repo=project",
			"--jq", ".data.repository.nameWithOwner")
		if got := strings.TrimSpace(observed.output); got != "partner/project" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer partner-secret")
	})

	t.Run("GraphQL literal", func(t *testing.T) {
		query := `query { repository(owner:"related",name:"library") { nameWithOwner } }`
		observed := runGHCommand(t, gh, "client-secret", `{"data":{"repository":{"nameWithOwner":"related/library"}}}`,
			"api", "graphql", "-f", "query="+query, "--jq", ".data.repository.nameWithOwner")
		if got := strings.TrimSpace(observed.output); got != "related/library" {
			t.Fatalf("gh output = %q", got)
		}
		assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer related-secret")
	})

	t.Run("explicit credential hint", func(t *testing.T) {
		observed := runGHCommand(t, gh, "client-secret:partner", `{"login":"octocat"}`,
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
		observed := runGHCommandResult(t, gh, "client-secret", `{"data":{}}`,
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
		{name: "unconditional route", repository: "public/example", token: "main-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientAuthorization := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:client-secret"))
			result := runProxiedCommandResult(t, git,
				[]string{
					"-c", "protocol.version=0",
					"-c", "http.https://github.com/.extraHeader=" + clientAuthorization,
					"ls-remote", "https://github.com/" + test.repository + ".git",
				},
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

func TestGHSearchRoutingE2E(t *testing.T) {
	gh := ghE2EBinary(t)
	tests := []struct {
		name, path, token, qualifier string
		args                         []string
	}{
		{name: "code repo", path: "/search/code", token: "related-secret", qualifier: "repo:related/library", args: []string{"search", "code", "panic", "--repo", "related/library", "--json", "path"}},
		{name: "commits repo", path: "/search/commits", token: "related-secret", qualifier: "repo:related/library", args: []string{"search", "commits", "fix", "--repo", "related/library", "--json", "sha"}},
		{name: "issues repo", path: "/search/issues", token: "related-secret", qualifier: "repo:related/library", args: []string{"search", "issues", "bug", "--repo", "related/library", "--json", "number"}},
		{name: "issues NOT label", path: "/search/issues", token: "related-secret", qualifier: "NOT label:bug", args: []string{"search", "issues", "NOT", "label:bug", "--repo", "related/library", "--json", "number"}},
		{name: "prs repo", path: "/search/issues", token: "partner-secret", qualifier: "repo:partner/library", args: []string{"search", "prs", "fix", "--repo", "partner/library", "--json", "number"}},
		{name: "repos owner", path: "/search/repositories", token: "partner-secret", qualifier: "user:partner", args: []string{"search", "repos", "--owner", "partner", "--json", "fullName"}},
		{name: "code owner", path: "/search/code", token: "related-secret", qualifier: "user:related", args: []string{"search", "code", "panic", "--owner", "related", "--json", "path"}},
		{name: "raw qualifier", path: "/search/issues", token: "related-secret", qualifier: "repo:related/library", args: []string{"search", "issues", "repo:related/library", "--json", "number"}},
		{name: "unfinished other qualifier", path: "/search/issues", token: "related-secret", qualifier: `label:"bug`, args: []string{"api", "--method", "GET", "search/issues", "-f", `q=repo:related/library label:"bug`, "--jq", ".items"}},
		{name: "multiple repos", path: "/search/issues", token: "related-secret", qualifier: "repo:related/two", args: []string{"search", "issues", "--repo", "related/one", "--repo", "related/two", "--json", "number"}},
		{name: "exact repository", path: "/search/code", token: "main-secret", qualifier: "repo:acme/main", args: []string{"search", "code", "panic", "--repo", "acme/main", "--json", "path"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := runGHCommand(t, gh, "client-secret", `{"total_count":0,"incomplete_results":false,"items":[]}`, test.args...)
			if strings.TrimSpace(observed.output) != "[]" {
				t.Fatalf("gh output = %q", observed.output)
			}
			requests := searchE2ERequests(t, observed.requests)
			assertSingleUpstreamAuthorization(t, requests, test.path, "Bearer "+test.token)
			query, err := url.ParseQuery(requests[0].rawQuery)
			if err != nil || !strings.Contains(query.Get("q"), test.qualifier) {
				t.Fatalf("search query = %q, error = %v", query.Get("q"), err)
			}
		})
	}
	for _, command := range []string{"pr", "issue"} {
		t.Run(command+" list search", func(t *testing.T) {
			observed := runGHCommandResponding(t, gh, "client-secret", func(request e2eRequest) string {
				if e2eFeatureDetection(request) {
					return `{"data":{}}`
				}
				if strings.Contains(request.body, "hasIssuesEnabled") {
					return `{"data":{"repository":{"hasIssuesEnabled":true}}}`
				}
				return `{"data":{"search":{"issueCount":0,"nodes":[],"pageInfo":{"hasNextPage":false}}},"total_count":0,"items":[]}`
			},
				command, "list", "--repo", "related/library", "--search", "bug", "--json", "number")
			if observed.err != nil {
				t.Fatalf("gh failed: %v\n%s", observed.err, observed.output)
			}
			requests := searchE2ERequests(t, observed.requests)
			if strings.TrimSpace(observed.output) != "[]" || len(requests) == 0 {
				t.Fatalf("gh output = %q, requests = %d", observed.output, len(observed.requests))
			}
			var searches []e2eRequest
			for _, request := range requests {
				if request.authorization != "Bearer related-secret" || (request.path != "/graphql" && request.path != "/search/issues") {
					t.Fatalf("upstream = %s with %q", request.path, request.authorization)
				}
				if request.path == "/search/issues" || e2eGraphQLHasField(request, "search") {
					searches = append(searches, request)
				}
			}
			if len(searches) != 1 {
				t.Fatalf("search request count = %d, want 1", len(searches))
			}
		})
	}
	t.Run("GraphQL search variable", func(t *testing.T) {
		observed := runGHCommand(t, gh, "client-secret", `{"data":{"search":{"issueCount":0}}}`,
			"api", "graphql", "-f", "query=query($filter:String!){search(query:$filter,type:ISSUE){issueCount nodes{__typename}}}", "-f", "filter=repo:partner/library NOT label:bug", "--jq", ".data.search.issueCount")
		assertSingleUpstreamAuthorization(t, searchE2ERequests(t, observed.requests), "/graphql", "Bearer partner-secret")
	})
	for name, query := range map[string]string{
		"default search variable": `query($q:String!="repo:related/library"){repository(owner:"related",name:"library"){id} search(query:$q,type:ISSUE){issueCount}}`,
		"block search string":     `{repository(owner:"related",name:"library"){id} search(query:"""repo:related/library""",type:ISSUE){issueCount}}`,
	} {
		t.Run(name, func(t *testing.T) {
			observed := runGHCommand(t, gh, "client-secret", `{"data":{"search":{"issueCount":0},"repository":{"id":"test"}}}`,
				"api", "graphql", "-f", "query="+query, "--jq", ".data.search.issueCount")
			assertSingleUpstreamAuthorization(t, observed.requests, "/graphql", "Bearer related-secret")
		})
	}
	for _, query := range []string{"repo:related/one repo:partner/two", "repo:invalid", "repo:related/one NOT repo:partner/two"} {
		t.Run("reject REST "+query, func(t *testing.T) {
			observed := runGHCommandResult(t, gh, "client-secret", `{"items":[]}`,
				"search", "issues", query, "--json", "number")
			if observed.err == nil || len(searchE2ERequests(t, observed.requests)) != 0 {
				t.Fatalf("error = %v, upstream requests = %d", observed.err, len(observed.requests))
			}
		})
		t.Run("reject GraphQL "+query, func(t *testing.T) {
			observed := runGHCommandResult(t, gh, "client-secret", `{}`,
				"api", "graphql", "-f", "query=query($q:String!){search(query:$q,type:ISSUE){issueCount}}", "-f", "q="+query)
			if observed.err == nil || len(observed.requests) != 0 {
				t.Fatalf("error = %v, upstream requests = %d", observed.err, len(observed.requests))
			}
		})
	}
}

func searchE2ERequests(t *testing.T, requests []e2eRequest) []e2eRequest {
	t.Helper()
	var searches []e2eRequest
	for _, request := range requests {
		if e2eFeatureDetection(request) {
			if request.authorization != "Bearer main-secret" {
				t.Fatalf("feature detection Authorization = %q, want unconditional credential", request.authorization)
			}
			continue
		}
		searches = append(searches, request)
	}
	return searches
}

func e2eFeatureDetection(request e2eRequest) bool {
	return e2eGraphQLHasField(request, "__schema") || e2eGraphQLHasField(request, "__type")
}

func e2eGraphQLHasField(request e2eRequest, field string) bool {
	if request.path != "/graphql" {
		return false
	}
	var payload struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(request.body), &payload) != nil {
		return false
	}
	tokens := graphQLTokens(payload.Query)
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].is(field) && (tokens[i+1].is("(") || tokens[i+1].is("{")) {
			return true
		}
	}
	return false
}

func TestE2EFeatureDetection(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{query: `{__schema {queryType{fields{name}}}}`, want: true},
		{query: `{__type(name:"Query"){fields{name}}}`, want: true},
		{query: `{search(query:$q,type:ISSUE){nodes{__typename}}}`},
		{query: `{search(query:"__type(name:x) __schema{q}",type:ISSUE){issueCount}}`},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			request := e2eRequest{path: "/graphql", body: graphQLSearchBody(t, test.query, nil)}
			if got := e2eFeatureDetection(request); got != test.want {
				t.Fatalf("feature detection = %v, want %v", got, test.want)
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
	return runGHCommandResponding(t, gh, hint, func(e2eRequest) string { return responseBody }, args...)
}

func runGHCommandResponding(t *testing.T, gh, hint string, respond func(e2eRequest) string, args ...string) e2eResult {
	t.Helper()
	clientHome := t.TempDir()
	return runProxiedCommandResponding(t, gh, args, []string{
		"GH_CONFIG_DIR=" + filepath.Join(clientHome, "config"),
		"GH_HOST=github.com",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
		"GH_TOKEN=" + hint,
		"HOME=" + clientHome,
		"XDG_STATE_HOME=" + filepath.Join(clientHome, "state"),
	}, "application/json", respond)
}

func runProxiedCommandResult(t *testing.T, executable string, args, environment []string, contentType, responseBody string) e2eResult {
	t.Helper()
	return runProxiedCommandResponding(t, executable, args, environment, contentType, func(e2eRequest) string { return responseBody })
}

func runProxiedCommandResponding(t *testing.T, executable string, args, environment []string, contentType string, respond func(e2eRequest) string) e2eResult {
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
		request := e2eRequest{
			host:          req.URL.Host,
			path:          req.URL.Path,
			rawQuery:      req.URL.RawQuery,
			authorization: req.Header.Get("Authorization"),
			body:          string(body),
		}
		requests = append(requests, request)
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(respond(request))),
			Request:    req,
		}, nil
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := newProxy(testConfig(), ca, upstream, logger)
	proxyServer := httptest.NewServer(proxy.Handler())
	t.Cleanup(proxyServer.Close)

	command := exec.Command(executable, args...)
	// actions/checkout and developer repositories can add GitHub Authorization
	// headers to the local Git config. Run outside the checkout so the E2E client
	// sees only the configuration supplied by this test.
	command.Dir = t.TempDir()
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
