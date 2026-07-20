package ghrouter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", "client-secret")
	t.Setenv("TEST_MAIN_TOKEN", "main-secret")
	t.Setenv("TEST_READ_TOKEN", "read-secret")
	path := writeTestConfig(t, `{
  "version": 1,
  "server": {
    "listen": "127.0.0.1:8080",
    "caCertificate": "ca.pem",
    "caPrivateKey": "ca-key.pem"
  },
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {
    "routes": [
      {"when": {"repository": "Acme/Main"}, "credential": "main"},
      {"when": {"owner": "Related"}, "credential": "read"},
      {"credential": "main"}
    ]
  },
  "credentials": [
    {"id": "main", "tokenEnv": "TEST_MAIN_TOKEN", "hints": ["main"]},
    {"id": "read", "tokenEnv": "TEST_READ_TOKEN", "hints": ["read", "related"]}
  ]
}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Authentication.token != "client-secret" {
		t.Fatal("proxy access token was not loaded from the environment")
	}
	if cfg.Routing.Routes[0].When.Repository != "acme/main" || cfg.Routing.Routes[1].When.Owner != "related" {
		t.Fatalf("routes were not canonicalized: %#v", cfg.Routing.Routes)
	}
	if cfg.Routing.Routes[2].When != nil {
		t.Fatal("final route is not unconditional")
	}
	if cfg.Credentials[0].token != "main-secret" || cfg.Credentials[1].token != "read-secret" {
		t.Fatal("GitHub tokens were not loaded from the environment")
	}
}

func TestLoadConfigAllowsGeneratedCA(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", "client-secret")
	t.Setenv("TEST_TOKEN", "secret")
	path := writeTestConfig(t, `{
  "version": 1,
  "server": {"listen": "127.0.0.1:8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": []},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Server.CACertificate != "" || cfg.Server.CAPrivateKey != "" {
		t.Fatal("generated CA configuration unexpectedly has file paths")
	}
}

func TestLoadConfigRejectsInvalidInput(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", "client-secret")
	t.Setenv("TEST_CLIENT_TOKEN_WITH_COLON", "client:secret")
	t.Setenv("TEST_TOKEN", "secret")
	t.Setenv("TEST_OTHER_TOKEN", "other-secret")

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "legacy GitHub hosts field",
			content: `{
  "version": 1,
  "github": {"hosts": ["api.github.com"]}
}`,
			want: "unknown field",
		},
		{
			name: "only CA certificate path",
			content: `{
  "version": 1,
  "server": {"listen": ":8080", "caCertificate": "ca.pem"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": []},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "must be specified together",
		},
		{
			name: "unknown route credential",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": [{"when": {"owner": "acme"}, "credential": "missing"}]},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "unknown credential",
		},
		{
			name: "unconditional route is not final",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": [
    {"credential": "main"},
    {"when": {"owner": "acme"}, "credential": "main"}
  ]},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "must be the final route",
		},
		{
			name: "route condition has repository and owner",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": [
    {"when": {"repository": "acme/main", "owner": "acme"}, "credential": "main"}
  ]},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "exactly one",
		},
		{
			name: "duplicate credential hint",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": []},
  "credentials": [
    {"id": "main", "tokenEnv": "TEST_TOKEN", "hints": ["same"]},
    {"id": "other", "tokenEnv": "TEST_OTHER_TOKEN", "hints": ["same"]}
  ]
}`,
			want: "duplicate credential hint",
		},
		{
			name: "proxy token contains delimiter",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN_WITH_COLON"},
  "routing": {"routes": []},
  "credentials": [{"id": "main", "tokenEnv": "TEST_TOKEN"}]
}`,
			want: "must not contain ':'",
		},
		{
			name: "proxy token equals GitHub token",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": []},
  "credentials": [{"id": "main", "tokenEnv": "TEST_CLIENT_TOKEN"}]
}`,
			want: "must differ from the proxy access token",
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
