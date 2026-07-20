package ghrouter

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRouterSelectsCredential(t *testing.T) {
	router := NewRouter(testConfig())

	tests := []struct {
		name       string
		host       string
		method     string
		path       string
		body       string
		auth       string
		credential string
		target     string
	}{
		{
			name:       "exact repository route wins",
			host:       "api.github.com",
			method:     http.MethodGet,
			path:       "/repos/acme/main/pulls",
			auth:       "token client-secret",
			credential: "main",
			target:     "acme/main",
		},
		{
			name:       "owner route",
			host:       "api.github.com",
			method:     http.MethodGet,
			path:       "/repos/RELATED/example",
			credential: "related-read",
			target:     "related/example",
		},
		{
			name:       "explicit hint wins",
			host:       "api.github.com",
			method:     http.MethodPost,
			path:       "/graphql",
			body:       `{"query":"mutation { node(id: \"opaque\") { id } }"}`,
			auth:       "Bearer client-secret:related",
			credential: "related-read",
			target:     "hint",
		},
		{
			name:       "unconditional route",
			host:       "api.github.com",
			method:     http.MethodGet,
			path:       "/user",
			credential: "main",
			target:     "unconditional",
		},
		{
			name:       "GraphQL variables",
			host:       "api.github.com",
			method:     http.MethodPost,
			path:       "/graphql",
			body:       `{"query":"query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){id}}","variables":{"owner":"related","repo":"private"}}`,
			auth:       "token client-secret",
			credential: "related-read",
			target:     "related/private",
		},
		{
			name:       "GraphQL literal",
			host:       "api.github.com",
			method:     http.MethodPost,
			path:       "/graphql",
			body:       `{"query":"query { repository(owner: \"partner\", name: \"private\") { id } }"}`,
			credential: "partner-read",
			target:     "partner/private",
		},
		{
			name:       "Git smart HTTP",
			host:       "github.com",
			method:     http.MethodPost,
			path:       "/related/example.git/git-upload-pack",
			credential: "related-read",
			target:     "related/example",
		},
		{
			name:       "Basic hint",
			host:       "api.github.com",
			method:     http.MethodGet,
			path:       "/user",
			auth:       "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:client-secret:partner")),
			credential: "partner-read",
			target:     "hint",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := newRouterRequest(t, test.method, test.path, test.body)
			if test.auth != "" {
				req.Header.Set("Authorization", test.auth)
			}
			selection, err := router.Select(req, test.host)
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if selection.CredentialID != test.credential || selection.Target != test.target {
				t.Fatalf("Select() = credential %q target %q, want %q %q", selection.CredentialID, selection.Target, test.credential, test.target)
			}
		})
	}
}

func TestRouterRejectsUnsafeOrAmbiguousRequests(t *testing.T) {
	router := NewRouter(testConfig())

	tests := []struct {
		name   string
		host   string
		method string
		path   string
		body   string
		auth   string
		cookie string
		code   string
	}{
		{
			name:   "unknown client token",
			host:   "api.github.com",
			method: http.MethodGet,
			path:   "/user",
			auth:   "Bearer github_pat_attacker",
			code:   "client_authentication_rejected",
		},
		{
			name:   "unknown credential hint",
			host:   "api.github.com",
			method: http.MethodGet,
			path:   "/user",
			auth:   "Bearer client-secret:unknown",
			code:   "unknown_hint",
		},
		{
			name:   "cookie",
			host:   "api.github.com",
			method: http.MethodGet,
			path:   "/user",
			cookie: "user_session=attacker",
			code:   "client_credential_rejected",
		},
		{
			name:   "token query parameter",
			host:   "api.github.com",
			method: http.MethodGet,
			path:   "/user?access_token=attacker",
			code:   "client_credential_rejected",
		},
		{
			name:   "ambiguous GraphQL route",
			host:   "api.github.com",
			method: http.MethodPost,
			path:   "/graphql",
			body:   `{"query":"query { a: repository(owner: \"related\", name: \"one\") { id } b: repository(owner: \"partner\", name: \"two\") { id } }"}`,
			code:   "ambiguous_route",
		},
		{
			name:   "non Git github.com path",
			host:   "github.com",
			method: http.MethodGet,
			path:   "/acme/main",
			code:   "unsupported_request",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := newRouterRequest(t, test.method, test.path, test.body)
			if test.auth != "" {
				req.Header.Set("Authorization", test.auth)
			}
			if test.cookie != "" {
				req.Header.Set("Cookie", test.cookie)
			}
			_, err := router.Select(req, test.host)
			var requestErr *RequestError
			if !errors.As(err, &requestErr) {
				t.Fatalf("Select() error = %v, want RequestError", err)
			}
			if requestErr.Code != test.code {
				t.Fatalf("Select() code = %q, want %q", requestErr.Code, test.code)
			}
		})
	}
}

func TestGraphQLBodyIsRestored(t *testing.T) {
	router := NewRouter(testConfig())
	body := `{"query":"query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){id}}","variables":{"owner":"related","repo":"private"}}`
	req := newRouterRequest(t, http.MethodPost, "/graphql", body)

	if _, err := router.Select(req, "api.github.com"); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", restored, body)
	}
}

func TestRouterRejectsMissingRouteWithoutDefault(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Routes = cfg.Routing.Routes[:len(cfg.Routing.Routes)-1]
	router := NewRouter(cfg)
	req := newRouterRequest(t, http.MethodGet, "/user", "")

	_, err := router.Select(req, "api.github.com")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Code != "route_not_found" {
		t.Fatalf("Select() error = %v, want route_not_found", err)
	}
}

func TestRouterRejectsMissingAuthentication(t *testing.T) {
	router := NewRouter(testConfig())
	req := newRouterRequest(t, http.MethodGet, "/user", "")
	req.Header.Del("Authorization")

	_, err := router.Select(req, "api.github.com")
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Status != http.StatusUnauthorized || requestErr.Code != "client_authentication_rejected" {
		t.Fatalf("Select() error = %v, want unauthorized client_authentication_rejected", err)
	}
}

func TestRouterEvaluatesRoutesInOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Routes = []RouteConfig{
		{When: &RouteCondition{Owner: "acme"}, Credential: "related-read"},
		{When: &RouteCondition{Repository: "acme/main"}, Credential: "main"},
		{Credential: "main"},
	}
	router := NewRouter(cfg)
	req := newRouterRequest(t, http.MethodGet, "/repos/acme/main", "")

	selection, err := router.Select(req, "api.github.com")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.CredentialID != "related-read" {
		t.Fatalf("credential = %q, want first matching owner route", selection.CredentialID)
	}
}

func newRouterRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token client-secret")
	return req
}

func testConfig() *Config {
	return &Config{
		Authentication: AuthenticationConfig{token: "client-secret"},
		Routing: RoutingConfig{
			Routes: []RouteConfig{
				{When: &RouteCondition{Repository: "acme/main"}, Credential: "main"},
				{When: &RouteCondition{Owner: "related"}, Credential: "related-read"},
				{When: &RouteCondition{Owner: "partner"}, Credential: "partner-read"},
				{Credential: "main"},
			},
		},
		Credentials: []Credential{
			{ID: "main", Hints: []string{"main"}, token: "main-secret"},
			{ID: "related-read", Hints: []string{"related", "related-read"}, token: "related-secret"},
			{ID: "partner-read", Hints: []string{"partner"}, token: "partner-secret"},
		},
	}
}
