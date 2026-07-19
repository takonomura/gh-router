package ghrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxConfigSize = 1 << 20

type Config struct {
	Version     int           `json:"version"`
	Server      ServerConfig  `json:"server"`
	GitHub      GitHubConfig  `json:"github"`
	Routing     RoutingConfig `json:"routing"`
	Credentials []Credential  `json:"credentials"`
}

type ServerConfig struct {
	Listen        string `json:"listen"`
	CACertificate string `json:"caCertificate"`
	CAPrivateKey  string `json:"caPrivateKey"`
}

type GitHubConfig struct {
	Hosts []string `json:"hosts"`
}

type RoutingConfig struct {
	AutoHint          string            `json:"autoHint"`
	DefaultCredential string            `json:"defaultCredential"`
	CredentialHints   map[string]string `json:"credentialHints"`
	Routes            []RouteConfig     `json:"routes"`
}

type RouteConfig struct {
	Repository string `json:"repository,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Credential string `json:"credential"`
}

type Credential struct {
	ID       string `json:"id"`
	TokenEnv string `json:"tokenEnv"`
	token    string
}

func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(io.LimitReader(f, maxConfigSize))
	dec.DisallowUnknownFields()

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if err := cfg.validateAndLoadTokens(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("decode config: multiple JSON values")
	}
	return fmt.Errorf("decode config: %w", err)
}

func (cfg *Config) validateAndLoadTokens() error {
	if cfg.Version != 1 {
		return fmt.Errorf("config: unsupported version %d", cfg.Version)
	}
	if strings.TrimSpace(cfg.Server.Listen) == "" {
		return errors.New("config: server.listen is required")
	}
	if cfg.Server.CACertificate == "" || cfg.Server.CAPrivateKey == "" {
		return errors.New("config: server CA certificate and private key are required")
	}
	if strings.TrimSpace(cfg.Routing.AutoHint) == "" {
		return errors.New("config: routing.autoHint is required")
	}

	allowedHostNames := map[string]bool{
		"api.github.com": true,
		"github.com":     true,
	}
	seenHosts := make(map[string]bool)
	for i, host := range cfg.GitHub.Hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if !allowedHostNames[host] {
			return fmt.Errorf("config: github.hosts[%d] is not supported: %q", i, host)
		}
		if seenHosts[host] {
			return fmt.Errorf("config: duplicate GitHub host %q", host)
		}
		seenHosts[host] = true
		cfg.GitHub.Hosts[i] = host
	}
	if len(seenHosts) == 0 {
		return errors.New("config: at least one GitHub host is required")
	}

	credentialIDs := make(map[string]bool)
	for i := range cfg.Credentials {
		credential := &cfg.Credentials[i]
		if credential.ID == "" || credential.TokenEnv == "" {
			return fmt.Errorf("config: credentials[%d] requires id and tokenEnv", i)
		}
		if credentialIDs[credential.ID] {
			return fmt.Errorf("config: duplicate credential %q", credential.ID)
		}
		credentialIDs[credential.ID] = true

		token, ok := os.LookupEnv(credential.TokenEnv)
		if !ok || strings.TrimSpace(token) == "" {
			return fmt.Errorf("config: environment variable %s is empty or unset", credential.TokenEnv)
		}
		if token != strings.TrimSpace(token) {
			return fmt.Errorf("config: environment variable %s contains surrounding whitespace", credential.TokenEnv)
		}
		credential.token = token
	}
	if len(credentialIDs) == 0 {
		return errors.New("config: at least one credential is required")
	}

	if cfg.Routing.DefaultCredential != "" && !credentialIDs[cfg.Routing.DefaultCredential] {
		return fmt.Errorf("config: default credential %q does not exist", cfg.Routing.DefaultCredential)
	}
	for hint, credentialID := range cfg.Routing.CredentialHints {
		if strings.TrimSpace(hint) == "" {
			return errors.New("config: credential hint must not be empty")
		}
		if hint == cfg.Routing.AutoHint {
			return errors.New("config: auto hint conflicts with a credential hint")
		}
		if !credentialIDs[credentialID] {
			return fmt.Errorf("config: hint %q references unknown credential %q", hint, credentialID)
		}
	}

	seenRepositories := make(map[string]bool)
	seenOwners := make(map[string]bool)
	for i := range cfg.Routing.Routes {
		route := &cfg.Routing.Routes[i]
		if !credentialIDs[route.Credential] {
			return fmt.Errorf("config: routes[%d] references unknown credential %q", i, route.Credential)
		}
		if (route.Repository == "") == (route.Owner == "") {
			return fmt.Errorf("config: routes[%d] must set exactly one of repository or owner", i)
		}
		if route.Repository != "" {
			repository, ok := canonicalRepository(route.Repository)
			if !ok {
				return fmt.Errorf("config: routes[%d] has invalid repository %q", i, route.Repository)
			}
			if seenRepositories[repository] {
				return fmt.Errorf("config: duplicate repository route %q", repository)
			}
			seenRepositories[repository] = true
			route.Repository = repository
			continue
		}

		owner, ok := canonicalName(route.Owner)
		if !ok {
			return fmt.Errorf("config: routes[%d] has invalid owner %q", i, route.Owner)
		}
		if seenOwners[owner] {
			return fmt.Errorf("config: duplicate owner route %q", owner)
		}
		seenOwners[owner] = true
		route.Owner = owner
	}
	return nil
}
