package ghrouter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_MAIN_TOKEN", "main-secret")
	t.Setenv("TEST_READ_TOKEN", "read-secret")
	path := writeTestConfig(t, `{
  "version": 1,
  "server": {
    "listen": "127.0.0.1:8080",
    "caCertificate": "ca.pem",
    "caPrivateKey": "ca-key.pem"
  },
  "github": {"hosts": ["API.GITHUB.COM", "github.com"]},
  "routing": {
    "autoHint": "auto",
    "defaultCredential": "main",
    "credentialHints": {"use-read": "read"},
    "routes": [
      {"repository": "Acme/Main", "credential": "main"},
      {"owner": "Related", "credential": "read"}
    ]
  },
  "credentials": [
    {"id": "main", "tokenEnv": "TEST_MAIN_TOKEN"},
    {"id": "read", "tokenEnv": "TEST_READ_TOKEN"}
  ]
}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.GitHub.Hosts[0] != "api.github.com" {
		t.Fatalf("host = %q", cfg.GitHub.Hosts[0])
	}
	if cfg.Routing.Routes[0].Repository != "acme/main" || cfg.Routing.Routes[1].Owner != "related" {
		t.Fatalf("routes were not canonicalized: %#v", cfg.Routing.Routes)
	}
	if cfg.Credentials[0].token != "main-secret" || cfg.Credentials[1].token != "read-secret" {
		t.Fatal("tokens were not loaded from the environment")
	}
}

func TestLoadConfigRejectsInvalidInput(t *testing.T) {
	t.Setenv("TEST_TOKEN", "secret")

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "unknown field",
			content: `{
  "version": 1,
  "unexpected": true
}`,
			want: "unknown field",
		},
		{
			name: "unknown route credential",
			content: `{
  "version": 1,
  "server": {"listen": ":8080", "caCertificate": "ca.pem", "caPrivateKey": "key.pem"},
  "github": {"hosts": ["api.github.com"]},
  "routing": {"autoHint": "auto", "routes": [{"owner": "acme", "credential": "missing"}]},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "unknown credential",
		},
		{
			name: "unsupported host",
			content: `{
  "version": 1,
  "server": {"listen": ":8080", "caCertificate": "ca.pem", "caPrivateKey": "key.pem"},
  "github": {"hosts": ["evil.example"]},
  "routing": {"autoHint": "auto"},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "not supported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfig(writeTestConfig(t, test.content))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
