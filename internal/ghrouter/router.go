package ghrouter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	maxGraphQLBody = 2 << 20
	maxTargets     = 32
)

var (
	repositoryOwnerFirst = regexp.MustCompile(`(?is)\brepository\s*\(\s*owner\s*:\s*"([a-z0-9_.-]+)"\s*,\s*name\s*:\s*"([a-z0-9_.-]+)"`)
	repositoryNameFirst  = regexp.MustCompile(`(?is)\brepository\s*\(\s*name\s*:\s*"([a-z0-9_.-]+)"\s*,\s*owner\s*:\s*"([a-z0-9_.-]+)"`)
	ownerLiteral         = regexp.MustCompile(`(?is)\b(?:organization|user)\s*\(\s*login\s*:\s*"([a-z0-9_.-]+)"`)
)

type Router struct {
	clientToken string
	hints       map[string]string
	routes      []runtimeRoute
	credentials map[string]runtimeCredential
}

type runtimeRoute struct {
	repository string
	owner      string
	credential string
}

type runtimeCredential struct {
	id    string
	token string
}

type Selection struct {
	CredentialID string
	Token        string
	Target       string
}

type RequestError struct {
	Status int
	Code   string
	Err    error
}

func (e *RequestError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func NewRouter(cfg *Config) *Router {
	router := &Router{
		clientToken: cfg.Authentication.token,
		hints:       make(map[string]string),
		routes:      make([]runtimeRoute, 0, len(cfg.Routing.Routes)),
		credentials: make(map[string]runtimeCredential, len(cfg.Credentials)),
	}
	for _, route := range cfg.Routing.Routes {
		runtimeRoute := runtimeRoute{credential: route.Credential}
		if route.When != nil {
			runtimeRoute.repository = route.When.Repository
			runtimeRoute.owner = route.When.Owner
		}
		router.routes = append(router.routes, runtimeRoute)
	}
	for _, credential := range cfg.Credentials {
		router.credentials[credential.ID] = runtimeCredential{id: credential.ID, token: credential.token}
		for _, hint := range credential.Hints {
			router.hints[hint] = credential.ID
		}
	}
	return router
}

func (r *Router) Select(req *http.Request, host string) (Selection, error) {
	if err := validateClientCredentialCarriers(req); err != nil {
		return Selection{}, err
	}

	credentialID, automatic, err := r.authenticate(req.Header.Values("Authorization"))
	if err != nil {
		return Selection{}, err
	}
	if !automatic {
		return r.selection(credentialID, "hint"), nil
	}

	var targets []routeTarget
	switch host {
	case apiGitHubHost:
		if req.URL.Path == "/graphql" {
			targets, err = extractGraphQLTargets(req)
		} else {
			targets, err = extractRESTTargets(req)
		}
	case gitHubHost:
		var target routeTarget
		target, err = extractGitTarget(req)
		if err == nil {
			targets = []routeTarget{target}
		}
	default:
		err = newRequestError(http.StatusForbidden, "host_denied", nil)
	}
	if err != nil {
		return Selection{}, err
	}

	credentialID, target, err := r.route(targets)
	if err != nil {
		return Selection{}, err
	}
	return r.selection(credentialID, target), nil
}

func (r *Router) authenticate(values []string) (credentialID string, automatic bool, err error) {
	if len(values) != 1 {
		return "", false, newRequestError(http.StatusUnauthorized, "client_authentication_rejected", errors.New("exactly one Authorization header is required"))
	}

	value, ok := authorizationValue(values[0])
	if !ok {
		return "", false, newRequestError(http.StatusUnauthorized, "client_authentication_rejected", errors.New("unsupported Authorization format"))
	}
	clientToken, hint, hasHint := strings.Cut(value, ":")
	if !secureTokenEqual(clientToken, r.clientToken) {
		return "", false, newRequestError(http.StatusUnauthorized, "client_authentication_rejected", errors.New("unknown token"))
	}
	if !hasHint {
		return "", true, nil
	}
	credentialID, ok = r.hints[hint]
	if !ok {
		return "", false, newRequestError(http.StatusForbidden, "unknown_hint", nil)
	}
	return credentialID, false, nil
}

func authorizationValue(header string) (string, bool) {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || value == "" {
		return "", false
	}
	value = strings.TrimSpace(value)
	switch strings.ToLower(scheme) {
	case "bearer", "token":
		return value, true
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", false
		}
		_, password, ok := strings.Cut(string(decoded), ":")
		return password, ok && password != ""
	default:
		return "", false
	}
}

func validateClientCredentialCarriers(req *http.Request) error {
	if len(req.Header.Values("Cookie")) != 0 {
		return newRequestError(http.StatusForbidden, "client_credential_rejected", errors.New("Cookie is not allowed"))
	}
	if req.URL.User != nil {
		return newRequestError(http.StatusForbidden, "client_credential_rejected", errors.New("URL userinfo is not allowed"))
	}
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return newRequestError(http.StatusBadRequest, "malformed_request", errors.New("invalid query string"))
	}
	for key := range query {
		switch strings.ToLower(key) {
		case "access_token", "client_secret":
			return newRequestError(http.StatusForbidden, "client_credential_rejected", fmt.Errorf("query parameter %q is not allowed", key))
		}
	}
	return nil
}

func (r *Router) route(targets []routeTarget) (credentialID, target string, err error) {
	if len(targets) == 0 {
		credentialID := r.routeTarget(routeTarget{})
		if credentialID == "" {
			return "", "", newRequestError(http.StatusForbidden, "route_not_found", nil)
		}
		return credentialID, "unconditional", nil
	}

	candidates := make(map[string]bool)
	labels := make([]string, 0, len(targets))
	for _, routeTarget := range targets {
		candidate := r.routeTarget(routeTarget)
		if candidate == "" {
			return "", "", newRequestError(http.StatusForbidden, "route_not_found", nil)
		}
		candidates[candidate] = true
		labels = append(labels, routeTarget.String())
	}
	if len(candidates) != 1 {
		return "", "", newRequestError(http.StatusForbidden, "ambiguous_route", nil)
	}
	sort.Strings(labels)
	for candidate := range candidates {
		return candidate, strings.Join(labels, ","), nil
	}
	return "", "", newRequestError(http.StatusForbidden, "route_not_found", nil)
}

func (r *Router) routeTarget(target routeTarget) string {
	for _, route := range r.routes {
		switch {
		case route.repository != "":
			if route.repository == target.repository {
				return route.credential
			}
		case route.owner != "":
			if route.owner == target.owner {
				return route.credential
			}
		default:
			return route.credential
		}
	}
	return ""
}

func (r *Router) selection(credentialID, target string) Selection {
	credential := r.credentials[credentialID]
	return Selection{CredentialID: credential.id, Token: credential.token, Target: target}
}

type routeTarget struct {
	owner      string
	repository string
}

func (t routeTarget) String() string {
	if t.repository != "" {
		return t.repository
	}
	return t.owner
}

func extractRESTTargets(req *http.Request) ([]routeTarget, error) {
	segments, err := pathSegments(req.URL)
	if err != nil {
		return nil, err
	}
	if len(segments) >= 3 && segments[0] == "repos" {
		repository, ok := canonicalRepository(segments[1] + "/" + segments[2])
		if !ok {
			return nil, newRequestError(http.StatusBadRequest, "malformed_request", errors.New("invalid repository path"))
		}
		owner, _, _ := strings.Cut(repository, "/")
		return []routeTarget{{owner: owner, repository: repository}}, nil
	}
	if len(segments) >= 2 && (segments[0] == "orgs" || segments[0] == "users") {
		owner, ok := canonicalName(segments[1])
		if !ok {
			return nil, newRequestError(http.StatusBadRequest, "malformed_request", errors.New("invalid owner path"))
		}
		return []routeTarget{{owner: owner}}, nil
	}
	return nil, nil
}

func extractGraphQLTargets(req *http.Request) ([]routeTarget, error) {
	if req.Body == nil || req.ContentLength == 0 {
		return nil, nil
	}
	if encoding := req.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, newRequestError(http.StatusBadRequest, "malformed_graphql", errors.New("compressed GraphQL body is not supported"))
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxGraphQLBody+1))
	if err != nil {
		return nil, newRequestError(http.StatusBadRequest, "malformed_graphql", errors.New("read GraphQL body"))
	}
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if len(body) > maxGraphQLBody {
		return nil, newRequestError(http.StatusRequestEntityTooLarge, "malformed_graphql", errors.New("GraphQL body is too large"))
	}

	var payload struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, newRequestError(http.StatusBadRequest, "malformed_graphql", errors.New("invalid GraphQL JSON"))
	}

	targets := make(map[string]routeTarget)
	owner, ownerOK := jsonString(payload.Variables["owner"])
	if ownerOK {
		if repository, ok := firstJSONString(payload.Variables, "repo", "name"); ok {
			addRepositoryTarget(targets, owner, repository)
		} else {
			addOwnerTarget(targets, owner)
		}
	}
	for _, match := range repositoryOwnerFirst.FindAllStringSubmatch(payload.Query, -1) {
		addRepositoryTarget(targets, match[1], match[2])
	}
	for _, match := range repositoryNameFirst.FindAllStringSubmatch(payload.Query, -1) {
		addRepositoryTarget(targets, match[2], match[1])
	}
	for _, match := range ownerLiteral.FindAllStringSubmatch(payload.Query, -1) {
		addOwnerTarget(targets, match[1])
	}
	if len(targets) > maxTargets {
		return nil, newRequestError(http.StatusBadRequest, "malformed_graphql", errors.New("too many routing targets"))
	}

	result := make([]routeTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	return result, nil
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, value != ""
}

func firstJSONString(values map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := jsonString(values[key]); ok {
			return value, true
		}
	}
	return "", false
}

func addRepositoryTarget(targets map[string]routeTarget, owner, name string) {
	repository, ok := canonicalRepository(owner + "/" + name)
	if !ok {
		return
	}
	canonicalOwner, _, _ := strings.Cut(repository, "/")
	targets["repo:"+repository] = routeTarget{owner: canonicalOwner, repository: repository}
}

func addOwnerTarget(targets map[string]routeTarget, value string) {
	owner, ok := canonicalName(value)
	if !ok {
		return
	}
	targets["owner:"+owner] = routeTarget{owner: owner}
}

func extractGitTarget(req *http.Request) (routeTarget, error) {
	segments, err := pathSegments(req.URL)
	if err != nil {
		return routeTarget{}, err
	}
	if len(segments) < 3 || !strings.HasSuffix(strings.ToLower(segments[1]), ".git") {
		return routeTarget{}, newRequestError(http.StatusForbidden, "unsupported_request", errors.New("not a Git smart HTTP path"))
	}

	validService := false
	if len(segments) == 4 && segments[2] == "info" && segments[3] == "refs" && req.Method == http.MethodGet {
		query, queryErr := url.ParseQuery(req.URL.RawQuery)
		if queryErr == nil && len(query) == 1 && len(query["service"]) == 1 {
			service := query.Get("service")
			validService = service == "git-upload-pack" || service == "git-receive-pack"
		}
	}
	if len(segments) == 3 && req.Method == http.MethodPost && req.URL.RawQuery == "" {
		validService = segments[2] == "git-upload-pack" || segments[2] == "git-receive-pack"
	}
	if !validService {
		return routeTarget{}, newRequestError(http.StatusForbidden, "unsupported_request", errors.New("unsupported Git smart HTTP request"))
	}

	repositoryName := segments[1][:len(segments[1])-4]
	repository, ok := canonicalRepository(segments[0] + "/" + repositoryName)
	if !ok {
		return routeTarget{}, newRequestError(http.StatusBadRequest, "malformed_request", errors.New("invalid Git repository path"))
	}
	owner, _, _ := strings.Cut(repository, "/")
	return routeTarget{owner: owner, repository: repository}, nil
}

func pathSegments(u *url.URL) ([]string, error) {
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(u.Path, "\\") || strings.Contains(u.Path, "//") {
		return nil, newRequestError(http.StatusBadRequest, "malformed_request", errors.New("ambiguous path"))
	}
	trimmed := strings.TrimPrefix(u.Path, "/")
	if trimmed == "" {
		return nil, nil
	}
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, newRequestError(http.StatusBadRequest, "malformed_request", errors.New("invalid path segment"))
		}
	}
	return segments, nil
}

func canonicalRepository(value string) (string, bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return "", false
	}
	owner, ownerOK := canonicalName(parts[0])
	repository, repositoryOK := canonicalName(parts[1])
	if !ownerOK || !repositoryOK {
		return "", false
	}
	return owner + "/" + repository, true
}

func canonicalName(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "." || value == ".." {
		return "", false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return "", false
	}
	return value, true
}

func newRequestError(status int, code string, err error) *RequestError {
	return &RequestError{Status: status, Code: code, Err: err}
}
