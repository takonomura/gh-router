package ghrouter

import (
	"errors"
	"fmt"
	"strings"
)

const SidecarCommand = "__sidecar"

const temporaryListenAddress = "127.0.0.1:0"

type sidecarReady struct {
	ProxyURL      string `json:"proxyURL,omitempty"`
	CACertificate string `json:"caCertificate,omitempty"`
	Error         string `json:"error,omitempty"`
}

func clientCredential(cfg *Config, hint string) (string, error) {
	if hint == "" {
		return cfg.Authentication.token, nil
	}
	for _, credential := range cfg.Credentials {
		for _, configuredHint := range credential.Hints {
			if hint == configuredHint {
				return cfg.Authentication.token + ":" + hint, nil
			}
		}
	}
	return "", fmt.Errorf("exec: unknown credential hint %q", hint)
}

func commandEnvironment(base []string, cfg *Config, credential, proxyURL, caCertificate string) []string {
	removedNames := map[string]bool{
		cfg.Authentication.TokenFrom.Env: true,
		"GH_TOKEN":                       true,
		"GITHUB_TOKEN":                   true,
		"GH_ENTERPRISE_TOKEN":            true,
		"GITHUB_ENTERPRISE_TOKEN":        true,
		"HTTPS_PROXY":                    true,
		"HTTP_PROXY":                     true,
		"NO_PROXY":                       true,
		"https_proxy":                    true,
		"http_proxy":                     true,
		"no_proxy":                       true,
		"SSL_CERT_FILE":                  true,
		"GH_HOST":                        true,
	}
	for _, configuredCredential := range cfg.Credentials {
		removedNames[configuredCredential.TokenFrom.Env] = true
	}

	environment := make([]string, 0, len(base)+9)
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || removedNames[name] || isConfiguredGitHubToken(cfg, value) {
			continue
		}
		environment = append(environment, entry)
	}
	for _, entry := range []string{
		"HTTPS_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"NO_PROXY=",
		"https_proxy=" + proxyURL,
		"http_proxy=" + proxyURL,
		"no_proxy=",
		"SSL_CERT_FILE=" + caCertificate,
		"GH_HOST=github.com",
		"GH_TOKEN=" + credential,
	} {
		environment = append(environment, entry)
	}
	return environment
}

func isConfiguredGitHubToken(cfg *Config, value string) bool {
	if value == "" {
		return false
	}
	for _, credential := range cfg.Credentials {
		if secureTokenEqual(value, credential.token) {
			return true
		}
	}
	return false
}

func validateReady(ready sidecarReady) error {
	if ready.Error != "" {
		return errors.New(ready.Error)
	}
	if ready.ProxyURL == "" || ready.CACertificate == "" {
		return errors.New("sidecar returned incomplete readiness information")
	}
	return nil
}

func executablePath(command string) (string, error) {
	if command == "" {
		return "", errors.New("exec: command is required")
	}
	return lookPath(command)
}
