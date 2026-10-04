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
		{name: "repository", query: "bug repo:acme/main", credential: "main", target: "acme/main"},
		{name: "owner through repository", query: "repo:RELATED/library", credential: "related-read", target: "related/library"},
		{name: "org", query: "org:related", credential: "related-read", target: "related"},
		{name: "user", query: "user:partner", credential: "partner-read", target: "partner"},
		{name: "quoted values", query: `repo:"related/library"`, credential: "related-read", target: "related/library"},
		{name: "quoted owner", query: `user:"RELATED"`, credential: "related-read", target: "related"},
		{name: "duplicate", query: "repo:related/library repo:RELATED/LIBRARY", credential: "related-read", target: "related/library"},
		{name: "same credential", query: "(repo:related/one OR repo:related/two) AND label:bug", credential: "related-read", target: "related/one,related/two"},
		{name: "whitespace", query: "bug\t repo:related/one\n\u3000repo:related/two", credential: "related-read", target: "related/one,related/two"},
		{name: "different credentials", query: "repo:related/one repo:partner/two", code: "ambiguous_route"},
		{name: "mixed repository and owner", query: "repo:related/one org:partner", code: "ambiguous_route"},
		{name: "unconditional", query: "bug author:related", credential: "main", target: "unconditional"},
		{name: "unconditional repository", query: "repo:public/example", credential: "main", target: "public/example"},
		{name: "empty", credential: "main", target: "unconditional"},
		{name: "quoted keyword", query: `"repo:partner/private" repo:related/one`, credential: "related-read", target: "related/one"},
		{name: "quoted other qualifier", query: `label:"repo:partner/private" repo:related/one`, credential: "related-read", target: "related/one"},
		{name: "escaped quotes", query: `"say \"repo:partner/private\"" repo:related/one`, credential: "related-read", target: "related/one"},
		{name: "excluded targets", query: "-repo:partner/private -org:partner -user:partner repo:related/one", credential: "related-read", target: "related/one"},
		{name: "only exclusion", query: "-repo:partner/private", credential: "main", target: "unconditional"},
		{name: "NOT unrelated qualifier", query: "repo:related/one NOT label:bug", credential: "related-read", target: "related/one"},
		{name: "NOT without target", query: "NOT label:bug", credential: "main", target: "unconditional"},
		{name: "NOT ignored for repository", query: "NOT repo:partner/two", credential: "partner-read", target: "partner/two"},
		{name: "NOT target conflict", query: "repo:related/one NOT repo:partner/two", code: "ambiguous_route"},
		{name: "unrelated excluded group", query: "repo:related/one -(label:bug)", credential: "related-read", target: "related/one"},
		{name: "excluded group target conflict", query: "repo:related/one -(repo:partner/two)", code: "ambiguous_route"},
		{name: "quoted NOT", query: `"NOT" repo:related/one`, credential: "related-read", target: "related/one"},
		{name: "unfinished other qualifier", query: `repo:related/one label:"bug`, credential: "related-read", target: "related/one"},
		{name: "unfinished quoted text", query: `repo:related/one "bug`, credential: "related-read", target: "related/one"},
		{name: "unfinished excluded qualifier", query: `repo:related/one -repo:"partner/two`, credential: "related-read", target: "related/one"},
		{name: "unfinished text without target", query: `label:"bug`, credential: "main", target: "unconditional"},
		{name: "invalid repo", query: "repo:related", code: "malformed_search"},
		{name: "invalid owner", query: "org:related/library", code: "malformed_search"},
		{name: "empty repo", query: "repo:", code: "malformed_search"},
		{name: "empty owner", query: `user:""`, code: "malformed_search"},
		{name: "truncated quote", query: `repo:"related/one`, code: "malformed_search"},
		{name: "truncated escape", query: `repo:"related/one\`, code: "malformed_search"},
		{name: "truncated owner", query: `org:"related`, code: "malformed_search"},
		{name: "trailing quoted text", query: `repo:"related/one"suffix`, code: "malformed_search"},
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
		{name: "literal", query: `{search(query:"repo:related/one",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "alias and reordered arguments", query: `{results:search(type:ISSUE,first:1,query:$filter){issueCount}}`, variables: map[string]any{"filter": "repo:partner/one", "other": "repo:related/one"}, credential: "partner-read"},
		{name: "multiple searches", query: `{a:search(query:"repo:related/one",type:ISSUE){issueCount} b:search(query:"repo:related/two",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "conflicting searches", query: `{a:search(query:"repo:related/one",type:ISSUE){issueCount} b:search(query:"repo:partner/two",type:ISSUE){issueCount}}`, code: "ambiguous_route"},
		{name: "existing target conflict", query: `{repository(owner:"acme",name:"main"){id} search(query:"repo:related/one",type:ISSUE){issueCount}}`, code: "ambiguous_route"},
		{name: "comment", query: "# search(query:\"repo:partner/one\")\n{search(query:\"repo:related/one\",type:ISSUE){issueCount}}", credential: "related-read"},
		{name: "CR comment", query: "# search(query:\"repo:partner/one\")\r{search(query:\"repo:related/one\",type:ISSUE){issueCount}}", credential: "related-read"},
		{name: "unrelated string", query: `{something(value:"search(query: repo:partner/one)"){id} search(query:"repo:related/one",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "unused variable", query: `{viewer{login}}`, variables: map[string]any{"search": "repo:partner/one"}, credential: "main"},
		{name: "unrelated block string", query: `{something(value:"""search(query:repo:partner/one)"""){id}}`, credential: "main"},
		{name: "missing variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, credential: "main"},
		{name: "null variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": nil}, credential: "main"},
		{name: "non-string variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": 42}, credential: "main"},
		{name: "empty variable", query: `{search(query:$filter,type:ISSUE){issueCount}}`, variables: map[string]any{"filter": ""}, credential: "main"},
		{name: "missing query", query: `{search(type:ISSUE){issueCount}}`, credential: "main"},
		{name: "duplicate query targets", query: `{search(query:"repo:related/one",query:"repo:partner/two"){issueCount}}`, code: "ambiguous_route"},
		{name: "non-string query", query: `{search(query:42){issueCount}}`, credential: "main"},
		{name: "default search with known repository", query: `query($filter:String!="repo:related/one"){repository(owner:"related",name:"one"){id} search(query:$filter,type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "block search with known repository", query: `{repository(owner:"related",name:"one"){id} search(query:"""repo:related/one""",type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "unknown search before known search", query: `{a:search(query:$missing,type:ISSUE){issueCount} b:search(query:"repo:partner/one",type:ISSUE){issueCount}}`, credential: "partner-read"},
		{name: "unknown search after known search", query: `{a:search(query:"repo:partner/one",type:ISSUE){issueCount} b:search(query:$missing,type:ISSUE){issueCount}}`, credential: "partner-read"},
		{name: "unknown nested search value", query: `{repository(owner:"related",name:"one"){id} search(query:{query:"repo:partner/two"},type:ISSUE){issueCount}}`, credential: "related-read"},
		{name: "block query", query: `{search(query:"""repo:related/one"""){issueCount}}`, credential: "main"},
		{name: "quoted variable name", query: `{search(query:$"filter"){issueCount}}`, variables: map[string]any{"filter": "repo:partner/one"}, credential: "main"},
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
			req := newRouterRequest(t, http.MethodGet, "/search/"+endpoint+"?q=repo:related/one", "")
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
				fmt.Fprintf(&query, "repo:related/r%d ", i)
			}
			if _, err := NewRouter(cfg).Select(request(query.String()), apiGitHubHost); err != nil {
				t.Fatal(err)
			}
			_, err = NewRouter(cfg).Select(request(query.String()+"repo:related/extra"), apiGitHubHost)
			assertSearchError(t, err, "malformed_search")
			req := request("repo:related/one NOT repo:partner/two")
			req.Header.Set("Authorization", "Bearer client-secret:partner")
			selection, err := NewRouter(cfg).Select(req, apiGitHubHost)
			if err != nil || selection.CredentialID != "partner-read" || selection.Target != "hint" {
				t.Fatalf("hint selection = %q %q, error = %v", selection.CredentialID, selection.Target, err)
			}
		})
	}
	t.Run("duplicate q", func(t *testing.T) {
		req := newRouterRequest(t, http.MethodGet, "/search/issues?q=repo:related/one&q=repo:partner/two", "")
		_, err := NewRouter(testConfig()).Select(req, apiGitHubHost)
		assertSearchError(t, err, "malformed_search")
	})
	t.Run("unknown GraphQL source without unconditional route", func(t *testing.T) {
		cfg := testConfig()
		cfg.Routes = cfg.Routes[:len(cfg.Routes)-1]
		query := `query($q:String!="repo:related/one"){search(query:$q,type:ISSUE){issueCount}}`
		req := newRouterRequest(t, http.MethodPost, "/graphql", graphQLSearchBody(t, query, nil))
		_, err := NewRouter(cfg).Select(req, apiGitHubHost)
		assertSearchError(t, err, "route_not_found")
	})
	t.Run("unrelated endpoint", func(t *testing.T) {
		req := newRouterRequest(t, http.MethodGet, "/user?q=repo:partner/one", "")
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
