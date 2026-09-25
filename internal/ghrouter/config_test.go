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
    "ca": {"certificateFile": "ca.pem", "privateKeyFile": "ca-key.pem"}
  },
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [
    {"when": {"repository": "Acme/Main"}, "credential": "main"},
    {"when": {"owner": "Related"}, "credential": "read"},
    {"credential": "main"}
  ],
  "credentials": [
    {"id": "main", "tokenFrom": {"env": "TEST_MAIN_TOKEN"}, "hints": ["main"]},
    {"id": "read", "tokenFrom": {"env": "TEST_READ_TOKEN"}, "hints": ["read", "related"]}
  ]
}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Authentication.token != "client-secret" {
		t.Fatal("proxy access token was not loaded from the environment")
	}
	if cfg.Routes[0].When.Repository != "acme/main" || cfg.Routes[1].When.Owner != "related" {
		t.Fatalf("routes were not canonicalized: %#v", cfg.Routes)
	}
	if cfg.Routes[2].When != nil {
		t.Fatal("final route is not unconditional")
	}
	if cfg.Credentials[0].token != "main-secret" || cfg.Credentials[1].token != "read-secret" {
		t.Fatal("GitHub tokens were not loaded from the environment")
	}
	if cfg.Server.CA.CertificateFile != filepath.Join(filepath.Dir(path), "ca.pem") || cfg.Server.CA.PrivateKeyFile != filepath.Join(filepath.Dir(path), "ca-key.pem") {
		t.Fatal("CA paths were not resolved relative to the configuration file")
	}
}

func TestLoadConfigDefaultsAndSharedToken(t *testing.T) {
	t.Setenv("TEST_SHARED_TOKEN", "shared-secret")
	for _, server := range []string{"", `"server": {},`, `"server": {"listen": "127.0.0.1:9000"},`} {
		cfg, err := LoadConfig(writeTestConfig(t, `{
  "version": 1, `+server+`
  "authentication": {"tokenFrom": {"env": "TEST_SHARED_TOKEN"}},
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_SHARED_TOKEN"}}]
}`))
		if err != nil {
			t.Fatal(err)
		}
		wantListen := "127.0.0.1:8080"
		if strings.Contains(server, "9000") {
			wantListen = "127.0.0.1:9000"
		}
		if cfg.Server.Listen != wantListen || cfg.Authentication.token != cfg.Credentials[0].token {
			t.Fatal("unexpected defaults or shared token")
		}
	}
}

func TestLoadConfigRejectsLegacyFieldsAndEmptyCA(t *testing.T) {
	for _, content := range []string{
		`{"authentication":{"tokenEnv":"TOKEN"}}`,
		`{"credentials":[{"id":"main","tokenEnv":"TOKEN"}]}`,
		`{"routing":{"routes":[]}}`,
		`{"server":{"caCertificate":"ca.pem"}}`,
		`{"server":{"caPrivateKey":"key.pem"}}`,
		`{"version":1,"server":{"ca":{}}}`,
	} {
		if _, err := LoadConfig(writeTestConfig(t, content)); err == nil {
			t.Fatalf("accepted invalid config: %s", content)
		}
	}
}

func TestLoadConfigAllowsGeneratedCA(t *testing.T) {
	t.Setenv("TEST_CLIENT_TOKEN", "client-secret")
	t.Setenv("TEST_TOKEN", "secret")
	path := writeTestConfig(t, `{
  "version": 1,
  "server": {"listen": "127.0.0.1:8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Server.CA != nil {
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
  "server": {"listen": ":8080", "ca": {"certificateFile": "ca.pem"}},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`,
			want: "must be specified together",
		},
		{
			name: "unknown route credential",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [{"when": {"owner": "acme"}, "credential": "missing"}],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`,
			want: "unknown credential",
		},
		{
			name: "unconditional route is not final",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [
    {"credential": "main"},
    {"when": {"owner": "acme"}, "credential": "main"}
  ],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`,
			want: "must be the final route",
		},
		{
			name: "route condition has repository and owner",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [
    {"when": {"repository": "acme/main", "owner": "acme"}, "credential": "main"}
  ],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`,
			want: "exactly one",
		},
		{
			name: "duplicate credential hint",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN"}},
  "routes": [],
  "credentials": [
    {"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}, "hints": ["same"]},
    {"id": "other", "tokenFrom": {"env": "TEST_OTHER_TOKEN"}, "hints": ["same"]}
  ]
}`,
			want: "duplicate credential hint",
		},
		{
			name: "proxy token contains delimiter",
			content: `{
  "version": 1,
  "server": {"listen": ":8080"},
  "authentication": {"tokenFrom": {"env": "TEST_CLIENT_TOKEN_WITH_COLON"}},
  "routes": [],
  "credentials": [{"id": "main", "tokenFrom": {"env": "TEST_TOKEN"}}]
}`,
			want: "must not contain ':'",
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
