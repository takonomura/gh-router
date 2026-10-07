package ghrouter

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGHRunLogRedirectE2E(t *testing.T) {
	gh := ghE2EBinary(t)
	const base = "/repos/octocat/main/actions"
	const job = `{"id":456,"run_id":123,"name":"build","status":"completed","conclusion":"success","steps":[{"name":"Test","number":1,"status":"completed","conclusion":"success"}]}`
	const run = `{"id":123,"workflow_id":789,"status":"completed","conclusion":"success","run_attempt":2,"run_started_at":"2026-01-01T00:00:00Z","jobs_url":"https://api.github.com/repos/octocat/main/actions/runs/123/jobs","html_url":"https://github.com/octocat/main/actions/runs/123"}`
	const signature = "sig=fake-signature%2B%2F&jwt=fake-jwt"
	for _, test := range []struct {
		name          string
		args          []string
		archiveLog    bool
		runLogPath    string
		wantDownloads int
	}{
		{"run archive", []string{"123"}, true, base + "/runs/123/logs", 1},
		{"run attempt", []string{"123", "--attempt", "2"}, true, base + "/runs/123/attempts/2/logs", 1},
		{"job log fallback", []string{"--job", "456"}, false, base + "/runs/123/logs", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			if test.archiveLog {
				file, err := writer.Create("build/1_Test.txt")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(file, "2026-01-01T00:00:00Z archive-log-marker\n"); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			respond := func(req e2eRequest) *http.Response {
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}}
				body := ""
				switch {
				case req.host == downloadHost && req.path == "/run.zip":
					response.Header.Set("Content-Type", "application/zip")
					body = archive.String()
				case req.host == downloadHost && req.path == "/job.txt":
					response.Header.Set("Content-Type", "text/plain")
					body = "2026-01-01T00:00:00Z job-log-marker\n"
				case req.host != apiGitHubHost:
					t.Errorf("unexpected upstream host %q", req.host)
					response.StatusCode = 404
				case req.path == test.runLogPath || req.path == base+"/jobs/456/logs":
					response.StatusCode = 302
					filename := "/run.zip"
					if req.path == base+"/jobs/456/logs" {
						filename = "/job.txt"
					}
					response.Header.Set("Location", "https://"+downloadHost+filename+"?"+signature)
				case req.path == base+"/jobs/456":
					body = job
				case req.path == base+"/runs/123" || req.path == base+"/runs/123/attempts/2":
					body = run
				case req.path == base+"/workflows/789":
					body = `{"id":789,"name":"CI"}`
				case req.path == base+"/runs/123/jobs" || req.path == base+"/runs/123/attempts/2/jobs":
					body = `{"total_count":1,"jobs":[` + job + `]}`
				default:
					t.Errorf("unexpected upstream path %q", req.path)
					response.StatusCode = 404
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response
			}
			args := append([]string{"run", "view", "--repo", "octocat/main", "--log"}, test.args...)
			result := runGHCommandRespondingHTTP(t, gh, "client-secret", respond, args...)
			if result.err != nil {
				t.Fatalf("gh run view failed: %v\n%s", result.err, result.output)
			}
			marker := "archive-log-marker"
			if !test.archiveLog {
				marker = "job-log-marker"
			}
			if !strings.Contains(result.output, marker) {
				t.Fatalf("gh did not display log: %s", result.output)
			}
			downloads, runLogRequests, jobLogRequests := 0, 0, 0
			for _, req := range result.requests {
				if req.proxyAuthorization != "" {
					t.Error("Proxy-Authorization reached upstream")
				}
				if req.host == apiGitHubHost {
					if req.authorization != "Bearer main-secret" {
						t.Error("API did not receive selected test credential")
					}
					if req.path == test.runLogPath {
						runLogRequests++
					}
					if req.path == base+"/jobs/456/logs" {
						jobLogRequests++
					}
				} else if req.host == downloadHost {
					downloads++
					if req.authorization != "" {
						t.Error("Authorization reached download upstream")
					}
					if req.rawQuery != signature {
						t.Error("signed query was rewritten")
					}
				}
			}
			if downloads != test.wantDownloads || runLogRequests != 1 {
				t.Fatalf("downloads = %d, run log requests = %d", downloads, runLogRequests)
			}
			if !test.archiveLog && jobLogRequests != 1 {
				t.Fatalf("job log requests = %d, want 1", jobLogRequests)
			}
		})
	}
}
