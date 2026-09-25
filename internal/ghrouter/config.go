package ghrouter

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxConfigSize = 1 << 20

type Config struct {
	Version        int                  `json:"version"`
	Server         ServerConfig         `json:"server"`
	Authentication AuthenticationConfig `json:"authentication"`
	Routes         []RouteConfig        `json:"routes"`
	Credentials    []Credential         `json:"credentials"`
}

type ServerConfig struct {
	Listen string    `json:"listen"`
	CA     *CAConfig `json:"ca,omitempty"`
}

type CAConfig struct {
	CertificateFile string `json:"certificateFile"`
	PrivateKeyFile  string `json:"privateKeyFile"`
}

type AuthenticationConfig struct {
	TokenFrom TokenSource `json:"tokenFrom"`
	token     string
}

type TokenSource struct {
	Env string `json:"env,omitempty"`
}

type RouteConfig struct {
	When       *RouteCondition `json:"when,omitempty"`
	Credential string          `json:"credential"`
}

type RouteCondition struct {
	Repository string `json:"repository,omitempty"`
	Owner      string `json:"owner,omitempty"`
}

type Credential struct {
	ID        string      `json:"id"`
	TokenFrom TokenSource `json:"tokenFrom"`
	Hints     []string    `json:"hints,omitempty"`
	token     string
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
	if cfg.Server.CA != nil {
		base, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("resolve config directory: %w", err)
		}
		cfg.Server.CA.CertificateFile = resolveConfigPath(base, cfg.Server.CA.CertificateFile)
		cfg.Server.CA.PrivateKeyFile = resolveConfigPath(base, cfg.Server.CA.PrivateKeyFile)
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
		cfg.Server.Listen = "127.0.0.1:8080"
	}
	if cfg.Server.CA != nil && (cfg.Server.CA.CertificateFile == "" || cfg.Server.CA.PrivateKeyFile == "") {
		return errors.New("config: server CA certificate and private key must be specified together")
	}

	clientToken, err := loadTokenEnvironment(cfg.Authentication.TokenFrom.Env, "authentication.tokenFrom.env")
	if err != nil {
		return err
	}
	if strings.Contains(clientToken, ":") {
		return errors.New("config: proxy access token must not contain ':'")
	}
	cfg.Authentication.token = clientToken

	credentialIDs := make(map[string]bool)
	seenHints := make(map[string]bool)
	for i := range cfg.Credentials {
		credential := &cfg.Credentials[i]
		if credential.ID == "" || credential.TokenFrom.Env == "" {
			return fmt.Errorf("config: credentials[%d] requires id and tokenFrom.env", i)
		}
		if credentialIDs[credential.ID] {
			return fmt.Errorf("config: duplicate credential %q", credential.ID)
		}
		credentialIDs[credential.ID] = true

		token, err := loadTokenEnvironment(credential.TokenFrom.Env, fmt.Sprintf("credentials[%d].tokenFrom.env", i))
		if err != nil {
			return err
		}
		credential.token = token

		for j, hint := range credential.Hints {
			if hint == "" || hint != strings.TrimSpace(hint) {
				return fmt.Errorf("config: credentials[%d].hints[%d] must not be empty or contain surrounding whitespace", i, j)
			}
			if strings.Contains(hint, ":") {
				return fmt.Errorf("config: credentials[%d].hints[%d] must not contain ':'", i, j)
			}
			if seenHints[hint] {
				return fmt.Errorf("config: duplicate credential hint %q", hint)
			}
			seenHints[hint] = true
		}
	}
	if len(credentialIDs) == 0 {
		return errors.New("config: at least one credential is required")
	}

	seenRepositories := make(map[string]bool)
	seenOwners := make(map[string]bool)
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		if !credentialIDs[route.Credential] {
			return fmt.Errorf("config: routes[%d] references unknown credential %q", i, route.Credential)
		}
		if route.When == nil {
			if i != len(cfg.Routes)-1 {
				return fmt.Errorf("config: routes[%d] without when must be the final route", i)
			}
			continue
		}
		if (route.When.Repository == "") == (route.When.Owner == "") {
			return fmt.Errorf("config: routes[%d].when must set exactly one of repository or owner", i)
		}
		if route.When.Repository != "" {
			repository, ok := canonicalRepository(route.When.Repository)
			if !ok {
				return fmt.Errorf("config: routes[%d] has invalid repository %q", i, route.When.Repository)
			}
			if seenRepositories[repository] {
				return fmt.Errorf("config: duplicate repository route %q", repository)
			}
			seenRepositories[repository] = true
			route.When.Repository = repository
			continue
		}

		owner, ok := canonicalName(route.When.Owner)
		if !ok {
			return fmt.Errorf("config: routes[%d] has invalid owner %q", i, route.When.Owner)
		}
		if seenOwners[owner] {
			return fmt.Errorf("config: duplicate owner route %q", owner)
		}
		seenOwners[owner] = true
		route.When.Owner = owner
	}
	return nil
}

func loadTokenEnvironment(name, field string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("config: %s is required", field)
	}
	token, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("config: environment variable %s is empty or unset", name)
	}
	if token != strings.TrimSpace(token) {
		return "", fmt.Errorf("config: environment variable %s contains surrounding whitespace", name)
	}
	return token, nil
}

func secureTokenEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func resolveConfigPath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
