//go:build linux || darwin

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runExecTokenTestHelper() int {
	switch os.Args[1] {
	case "__test_token_auth", "__test_token_github":
		path := os.Args[2]
		old, _ := os.ReadFile(path)
		if err := os.WriteFile(path, append(old, 'x'), 0o600); err != nil {
			return 2
		}
		if os.Args[1] == "__test_token_github" {
			fmt.Fprint(os.Stdout, "private-helper-output")
			fmt.Fprint(os.Stderr, "private-helper-error")
			return 3
		}
		fmt.Printf("client-%d\n", len(old)+1)
		return 0
	case "__test_token_client":
		ca, err := os.ReadFile(os.Getenv("SSL_CERT_FILE"))
		if err != nil {
			return 4
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return 5
		}
		proxyURL, err := url.Parse(os.Getenv("HTTPS_PROXY"))
		if err != nil {
			return 6
		}
		transport := &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		for i, want := range []int{401, 503, 503} {
			req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
			token := os.Getenv("GH_TOKEN")
			if i == 0 {
				token = "unknown"
			}
			req.Header.Set("Authorization", "Bearer "+token)
			response, err := client.Do(req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "proxy request failed")
				return 7
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != want || strings.Contains(string(data), "private-helper") {
				fmt.Fprintf(os.Stderr, "unexpected response status %d, want %d\n", response.StatusCode, want)
				return 8
			}
		}
		fmt.Print("ok")
		return 0
	}
	return 9
}

func TestExecTokenCommandsAreIsolatedAndAuthenticationRunsOnce(t *testing.T) {
	directory := t.TempDir()
	authCount := filepath.Join(directory, "auth-count")
	githubCount := filepath.Join(directory, "github-count")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"version": 1,
		"authentication": map[string]any{"tokenFrom": map[string]any{"command": map[string]any{
			"argv": []string{binary, "__test_token_auth", authCount},
		}}},
		"routes": []any{map[string]any{"credential": "main"}},
		"credentials": []any{map[string]any{
			"id": "main", "tokenFrom": map[string]any{"command": map[string]any{
				"argv": []string{binary, "__test_token_github", githubCount},
			}},
		}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "exec", "-config", path, "--", binary, "__test_token_client")
	cmd.Env = execTestEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec failed: %v\n%s", err, output)
	}
	// Sidecar rejection logs are allowed, helper stdout/stderr are not.
	if !strings.Contains(string(output), "ok") || strings.Contains(string(output), "private-helper") || strings.Contains(string(output), "client-1") {
		t.Fatalf("unexpected command output: %s", output)
	}
	for path, want := range map[string]string{authCount: "x", githubCount: "xx"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("helper invocation count: %q, %v", data, err)
		}
	}
}

func TestExecFileAuthenticationAndLargePipeTransfer(t *testing.T) {
	for _, length := range []int{20, 64 << 10} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "client-token"), []byte(strings.Repeat("x", length)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The 64 KiB limit applies to the raw file, including its newline.
			if length == 64<<10 {
				if err := os.WriteFile(filepath.Join(directory, "client-token"), []byte(strings.Repeat("x", length)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			config := `{"version":1,"authentication":{"tokenFrom":{"file":"client-token"}},"credentials":[{"id":"main","tokenFrom":{"file":"unused-missing-token"}}]}`
			path := filepath.Join(directory, "config.json")
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "exec", "-config", path, "--", "/bin/sh", "-c", `printf '%s' "${#GH_TOKEN}"`)
			cmd.Env = execTestEnvironment()
			output, err := cmd.CombinedOutput()
			if err != nil || string(output) != fmt.Sprint(length) {
				t.Fatalf("file authentication pipe: %v, %s", err, output)
			}
		})
	}
}
