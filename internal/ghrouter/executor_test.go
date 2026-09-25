package ghrouter

import (
	"os"
	"strings"
	"testing"
)

func TestClientCredential(t *testing.T) {
	cfg := testConfig()
	if got, err := clientCredential(cfg, ""); err != nil || got != "client-secret" {
		t.Fatalf("clientCredential() = %q, %v", got, err)
	}
	if got, err := clientCredential(cfg, "related"); err != nil || got != "client-secret:related" {
		t.Fatalf("clientCredential(related) = %q, %v", got, err)
	}
	if _, err := clientCredential(cfg, "unknown"); err == nil {
		t.Fatal("clientCredential(unknown) succeeded")
	}
}

func TestCommandEnvironmentIsolatesGitHubTokens(t *testing.T) {
	cfg := testConfig()
	cfg.Authentication.TokenFrom.Env = testString("TEST_CLIENT_TOKEN")
	cfg.Credentials[0].TokenFrom.Env = testString("TEST_MAIN_TOKEN")
	cfg.Credentials[1].TokenFrom.Env = testString("TEST_RELATED_TOKEN")
	cfg.Credentials[2].TokenFrom.Env = testString("TEST_PARTNER_TOKEN")

	environment := commandEnvironment([]string{
		"PATH=/usr/bin",
		"TEST_CLIENT_TOKEN=client-secret",
		"TEST_MAIN_TOKEN=main-secret",
		"TOKEN_ALIAS=related-secret",
		"GH_TOKEN=unknown-client-token",
		"GITHUB_TOKEN=unknown-github-token",
		"GH_PROMPT_DISABLED=1",
		"HTTPS_PROXY=http://old-proxy",
		"SSL_CERT_FILE=/old-ca.pem",
	}, cfg, "client-secret:related", "http://127.0.0.1:1234", "/tmp/ca.pem")

	values := environmentMap(environment)
	for _, name := range []string{
		"TEST_CLIENT_TOKEN",
		"TEST_MAIN_TOKEN",
		"TOKEN_ALIAS",
		"GITHUB_TOKEN",
	} {
		if _, ok := values[name]; ok {
			t.Fatalf("%s was retained in command environment", name)
		}
	}
	if values["GH_TOKEN"] != "client-secret:related" {
		t.Fatalf("GH_TOKEN = %q", values["GH_TOKEN"])
	}
	if values["HTTPS_PROXY"] != "http://127.0.0.1:1234" || values["https_proxy"] != "http://127.0.0.1:1234" {
		t.Fatalf("proxy environment = %#v", values)
	}
	if values["HTTP_PROXY"] != "http://127.0.0.1:1234" || values["http_proxy"] != "http://127.0.0.1:1234" {
		t.Fatalf("HTTP proxy environment = %#v", values)
	}
	if values["NO_PROXY"] != "" || values["no_proxy"] != "" {
		t.Fatalf("NO_PROXY environment = %#v", values)
	}
	if values["SSL_CERT_FILE"] != "/tmp/ca.pem" || values["GH_HOST"] != "github.com" {
		t.Fatalf("client environment = %#v", values)
	}
	if values["GH_PROMPT_DISABLED"] != "1" {
		t.Fatal("unrelated inherited environment was not preserved")
	}
	joined := strings.Join(environment, "\n")
	for _, token := range []string{"main-secret", "related-secret", "partner-secret", "unknown-github-token"} {
		if strings.Contains(joined, token) {
			t.Fatalf("command environment contains GitHub token %q", token)
		}
	}
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func TestExecutablePathRejectsEmptyCommand(t *testing.T) {
	if _, err := executablePath(""); err == nil {
		t.Fatal("executablePath(\"\") succeeded")
	}
	if path, err := executablePath(os.Args[0]); err != nil || path == "" {
		t.Fatalf("executablePath(test binary) = %q, %v", path, err)
	}
}

func testString(s string) *string { return &s }
