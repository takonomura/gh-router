package ghrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSearchRouting(t *testing.T) {
	tests := []struct {
		name, query, credential, target, code string
	}{
		{name: "repository", query: "bug repo:octocat/main", credential: "main", target: "octocat/main"},
		{name: "owner through repository", query: "repo:OCTO-ORG/library", credential: "related-read", target: "octo-org/library"},
		{name: "org", query: "org:octo-org", credential: "related-read", target: "octo-org"},
		{name: "user", query: "user:monalisa", credential: "partner-read", target: "monalisa"},
		{name: "quoted values", query: `repo:"octo-org/library"`, credential: "related-read", target: "octo-org/library"},
		{name: "quoted owner", query: `user:"OCTO-ORG"`, credential: "related-read", target: "octo-org"},
		{name: "duplicate", query: "repo:octo-org/library repo:OCTO-ORG/LIBRARY", credential: "related-read", target: "octo-org/library"},
		{name: "same credential", query: "(repo:octo-org/one OR repo:octo-org/two) AND label:bug", credential: "related-read", target: "octo-org/one,octo-org/two"},
		{name: "whitespace", query: "bug\t repo:octo-org/one\n\u3000repo:octo-org/two", credential: "related-read", target: "octo-org/one,octo-org/two"},
		{name: "different credentials", query: "repo:octo-org/one repo:monalisa/two", code: "ambiguous_route"},
		{name: "mixed repository and owner", query: "repo:octo-org/one org:monalisa", code: "ambiguous_route"},
		{name: "unconditional", query: "bug author:octo-org", credential: "main", target: "unconditional"},
		{name: "unconditional repository", query: "repo:octocat/example", credential: "main", target: "octocat/example"},
		{name: "empty", credential: "main", target: "unconditional"},
		{name: "quoted keyword", query: `"repo:monalisa/private" repo:octo-org/one`, credential: "related-read", target: "octo-org/one"},
		{name: "quoted other qualifier", query: `label:"repo:monalisa/private" repo:octo-org/one`, credential: "related-read", target: "octo-org/one"},
		{name: "escaped quotes", query: `"say \"repo:monalisa/private\"" repo:octo-org/one`, credential: "related-read", target: "octo-org/one"},
		{name: "excluded targets", query: "-repo:monalisa/private -org:monalisa -user:monalisa repo:octo-org/one", credential: "related-read", target: "octo-org/one"},
		{name: "only exclusion", query: "-repo:monalisa/private", credential: "main", target: "unconditional"},
		{name: "NOT unrelated qualifier", query: "repo:octo-org/one NOT label:bug", credential: "related-read", target: "octo-org/one"},
		{name: "NOT without target", query: "NOT label:bug", credential: "main", target: "unconditional"},
		{name: "NOT ignored for repository", query: "NOT repo:monalisa/two", credential: "partner-read", target: "monalisa/two"},
		{name: "NOT target conflict", query: "repo:octo-org/one NOT repo:monalisa/two", code: "ambiguous_route"},
		{name: "unrelated excluded group", query: "repo:octo-org/one -(label:bug)", credential: "related-read", target: "octo-org/one"},
		{name: "excluded group target conflict", query: "repo:octo-org/one -(repo:monalisa/two)", code: "ambiguous_route"},
		{name: "quoted NOT", query: `"NOT" repo:octo-org/one`, credential: "related-read", target: "octo-org/one"},
		{name: "unfinished other qualifier", query: `repo:octo-org/one label:"bug`, credential: "related-read", target: "octo-org/one"},
		{name: "unfinished quoted text", query: `repo:octo-org/one "bug`, credential: "related-read", target: "octo-org/one"},
		{name: "unfinished excluded qualifier", query: `repo:octo-org/one -repo:"monalisa/two`, credential: "related-read", target: "octo-org/one"},
		{name: "unfinished text without target", query: `label:"bug`, credential: "main", target: "unconditional"},
		{name: "invalid repo", query: "repo:octo-org", code: "malformed_search"},
		{name: "invalid owner", query: "org:octo-org/library", code: "malformed_search"},
		{name: "empty repo", query: "repo:", code: "malformed_search"},
		{name: "empty owner", query: `user:""`, code: "malformed_search"},
		{name: "truncated quote", query: `repo:"octo-org/one`, code: "malformed_search"},
		{name: "truncated escape", query: `repo:"octo-org/one\`, code: "malformed_search"},
		{name: "truncated owner", query: `org:"octo-org`, code: "malformed_search"},
		{name: "trailing quoted text", query: `repo:"octo-org/one"suffix`, code: "malformed_search"},
	}
	for _, test := range tests {
		for _, protocol := range []string{"REST", "GraphQL"} {
			t.Run(test.name+"/"+protocol, func(t *testing.T) {
				path := "/search/issues?q=" + url.QueryEscape(test.query)
				body := ""
				method := http.MethodGet
				if protocol == "GraphQL" {
					path = "/graphql"
					method = http.MethodPost
					body = graphQLSearchBody(t, `query($search:String!){search(first:1,query:$search,type:ISSUE){issueCount}}`, map[string]any{"search": test.query})
				}
				req := newRouterRequest(t, method, path, body)
				selection, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
				if test.code != "" {
					assertSearchError(t, err, test.code)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if selection.CredentialID != test.credential || selection.Target != test.target {
					t.Fatalf("selection = %q %q, want %q %q", selection.CredentialID, selection.Target, test.credential, test.target)
				}
				if req.URL.RawQuery != strings.TrimPrefix(path, "/search/issues?") && protocol == "REST" {
					t.Fatal("search query changed")
				}
				if protocol == "GraphQL" {
					restored, err := io.ReadAll(req.Body)
					if err != nil || string(restored) != body {
						t.Fatalf("GraphQL body changed: %v", err)
					}
				}
			})
		}
	}
}

func TestGraphQLSearchRouting(t *testing.T) {
	tests := []struct {
		name, query, credential, code string
		variables                     map[string]any
	}{
		{name: "literal", query: `{search(query:"repo:octo-org/one",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "alias and reordered arguments", query: `{results:search(type:ISSUE,first:1,query:$filter){issueCount}}`, variables: map[string]any{"filter": "repo:monalisa/one", "other": "repo:octo-org/one"}, credential: "partner-read"},
		{name: "multiple searches", query: `{a:search(query:"repo:octo-org/one",type:ISSUE){issueCount} b:search(query:"repo:octo-org/two",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "conflicting searches", query: `{a:search(query:"repo:octo-org/one",type:ISSUE){issueCount} b:search(query:"repo:monalisa/two",type:ISSUE){issueCount}}`, code: "ambiguous_route"},
		{name: "existing target conflict", query: `{repository(owner:"octocat",name:"main"){id} search(query:"repo:octo-org/one",type:ISSUE){issueCount}}`, code: "ambiguous_route"},
		{name: "comment", query: "# search(query:\"repo:monalisa/one\")\n{search(query:\"repo:octo-org/one\",type:ISSUE){issueCount}}", credential: "related-read"},
		{name: "CR comment", query: "# search(query:\"repo:monalisa/one\")\r{search(query:\"repo:octo-org/one\",type:ISSUE){issueCount}}", credential: "related-read"},
		{name: "unrelated string", query: `{something(value:"search(query: repo:monalisa/one)"){id} search(query:"repo:octo-org/one",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "unused variable", query: `{viewer{login}}`, variables: map[string]any{"search": "repo:monalisa/one"}, credential: "main"},
		{name: "unrelated block string", query: `{something(value:"""search(query:repo:monalisa/one)"""){id}}`, credential: "main"},
		{name: "missing variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, credential: "main"},
		{name: "null variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": nil}, credential: "main"},
		{name: "non-string variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": 42}, credential: "main"},
		{name: "empty variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": ""}, credential: "main"},
		{name: "missing query", query: `{search(type:ISSUE){issueCount}}`, credential: "main"},
		{name: "duplicate query targets", query: `{search(query:"repo:octo-org/one",query:"repo:monalisa/two"){issueCount}}`, code: "ambiguous_route"},
		{name: "non-string query", query: `{search(query:42){issueCount}}`, credential: "main"},
		{name: "default search with known repository", query: `query($filter:String!="repo:octo-org/one"){repository(owner:"octo-org",name:"one"){id} search(query:$filter,type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "block search with known repository", query: `{repository(owner:"octo-org",name:"one"){id} search(query:"""repo:octo-org/one""",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "unknown search before known search", query: `{a:search(query:$missing,type:ISSUE){issueCount} b:search(query:"repo:monalisa/one",type:ISSUE){issueCount}}`, credential: "partner-read"},
		{name: "unknown search after known search", query: `{a:search(query:"repo:monalisa/one",type:ISSUE){issueCount} b:search(query:$missing,type:ISSUE){issueCount}}`, credential: "partner-read"},
		{name: "unknown nested search value", query: `{repository(owner:"octo-org",name:"one"){id} search(query:{query:"repo:monalisa/two"},type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "block query", query: `{search(query:"""repo:octo-org/one"""){issueCount}}`, credential: "main"},
		{name: "quoted variable name", query: `{search(query:$"filter"){issueCount}}`, variables: map[string]any{"filter": "repo:monalisa/one"}, credential: "main"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := newRouterRequest(t, http.MethodPost, "/graphql", graphQLSearchBody(t, test.query, test.variables))
			selection, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
			if test.code != "" {
				assertSearchError(t, err, test.code)
			} else if err != nil || selection.CredentialID != test.credential {
				t.Fatalf("selection = %q, error = %v, want %q", selection.CredentialID, err, test.credential)
			}
		})
	}
}

func TestSearchRoutingBoundaries(t *testing.T) {
	for _, endpoint := range []string{"code", "commits", "issues", "repositories"} {
		t.Run(endpoint, func(t *testing.T) {
			req := newRouterRequest(t, http.MethodGet, "/search/"+endpoint+"?q=repo:octo-org/one", "")
			selection, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
			if err != nil || selection.CredentialID != "related-read" {
				t.Fatalf("selection = %q, error = %v", selection.CredentialID, err)
			}
		})
	}
	for _, protocol := range []string{"REST", "GraphQL"} {
		t.Run(protocol, func(t *testing.T) {
			request := func(query string) *http.Request {
				if protocol == "REST" {
					return newRouterRequest(t, http.MethodGet, "/search/issues?q="+url.QueryEscape(query), "")
				}
				return newRouterRequest(t, http.MethodPost, "/graphql", graphQLSearchBody(t, `{search(query:$q,type:ISSUE){issueCount}}`, map[string]any{"q": query}))
			}
			cfg := testConfig()
			cfg.Routes = cfg.Routes[:len(cfg.Routes)-1]
			_, err := NewRouter(cfg).Select(request("repo:unknown/one"), apiGitHubHost)
			assertSearchError(t, err, "route_not_found")
			_, err = NewRouter(cfg).Select(request("bug"), apiGitHubHost)
			assertSearchError(t, err, "route_not_found")
			var query strings.Builder
			for i := 0; i < maxTargets; i++ {
				fmt.Fprintf(&query, "repo:octo-org/r%d ", i)
			}
			if _, err := NewRouter(cfg).Select(request(query.String()), apiGitHubHost); err != nil {
				t.Fatal(err)
			}
			_, err = NewRouter(cfg).Select(request(query.String()+"repo:octo-org/extra"), apiGitHubHost)
			assertSearchError(t, err, "malformed_search")
			req := request("repo:octo-org/one NOT repo:monalisa/two")
			req.Header.Set("Authorization", "Bearer client-secret:partner")
			selection, err := NewRouter(cfg).Select(req, apiGitHubHost)
			if err != nil || selection.CredentialID != "partner-read" || selection.Target != "hint" {
				t.Fatalf("hint selection = %q %q, error = %v", selection.CredentialID, selection.Target, err)
			}
		})
	}
	t.Run("duplicate q", func(t *testing.T) {
		req := newRouterRequest(t, http.MethodGet, "/search/issues?q=repo:octo-org/one&q=repo:monalisa/two", "")
		_, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
		assertSearchError(t, err, "malformed_search")
	})
	t.Run("unknown GraphQL source without unconditional route", func(t *testing.T) {
		cfg := testConfig()
		cfg.Routes = cfg.Routes[:len(cfg.Routes)-1]
		query := `query($q:String!="repo:octo-org/one"){search(query:$q,type:ISSUE){issueCount}}`
		req := newRouterRequest(t, http.MethodPost, "/graphql", graphQLSearchBody(t, query, nil))
		_, err := NewRouter(cfg).Select(req, apiGitHubHost)
		assertSearchError(t, err, "route_not_found")
	})
	t.Run("unrelated endpoint", func(t *testing.T) {
		req := newRouterRequest(t, http.MethodGet, "/user?q=repo:monalisa/one", "")
		selection, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
		if err != nil || selection.Target != "unconditional" {
			t.Fatalf("selection = %q, error = %v", selection.Target, err)
		}
	})
}

func graphQLSearchBody(t *testing.T, query string, variables map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertSearchError(t *testing.T, err error, code string) {
	t.Helper()
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	if code == "malformed_search" && requestErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", requestErr.Status)
	}
}
