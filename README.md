# gh-router

`gh-router` is a small HTTPS forward proxy that keeps real GitHub tokens outside a sandbox and selects a Fine-grained PAT for each `gh` or Git request.

The MVP intentionally treats GitHub token scopes as the authorization boundary. The proxy only chooses a token, replaces the client's dummy credential, and forwards the request. See [the design](docs/design.md) for the supported use case and limitations.

## Build and test

Go 1.24 or newer is required. The project has no third-party dependencies.

```sh
go test ./...
go build -o gh-router ./cmd/gh-router
```

The test suite includes command-level E2E tests that run real `gh` and `git`
processes through a local proxy and a fake GitHub upstream. They verify TLS
interception, request extraction, credential selection, replacement, and
rejection without using a GitHub token or contacting GitHub.

`gh` tests are skipped when the binary is unavailable. Require the complete E2E
suite, as CI does, with:

```sh
GH_ROUTER_REQUIRE_GH_E2E=1 go test ./...
```

To use a `gh` binary outside `PATH`, set `GH_ROUTER_E2E_GH=/path/to/gh`.

## Setup

Copy [`config.example.json`](config.example.json), update the routes and token sources, and provide the real tokens only to the proxy process:

```sh
export GH_ROUTER_TOKEN_MAIN='github_pat_...'
export GH_ROUTER_CLIENT_TOKEN='a-separate-random-client-secret'
./gh-router -config config.json
```

When `server.ca` is omitted, the proxy generates an ephemeral CA in memory at startup. Its private key is never
written to disk or returned over HTTP. The CA is valid for 24 hours and a new
one is generated on every restart.

To use a persistent externally managed CA instead, set both file paths in the
configuration after generating one, for example:

```sh
openssl req -x509 -newkey rsa:3072 -nodes -days 30 \
  -keyout ca-key.pem \
  -out ca.pem \
  -subj '/CN=gh-router local CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
chmod 600 ca-key.pem
```

```json
"server": {
  "listen": "127.0.0.1:8080",
  "ca": {"certificateFile": "/etc/gh-router/ca.pem", "privateKeyFile": "/etc/gh-router/ca-key.pem"}
}
```

### Configuration and token sources

Both `authentication.tokenFrom` and `credentials[].tokenFrom` accept exactly
one of these sources:

```json
{"env": "GH_ROUTER_TOKEN"}
```

```json
{"file": "./secrets/github-token"}
```

```json
{
  "command": {
    "argv": ["token-helper", "get", "github"],
    "cacheTTL": "5m",
    "timeout": "30s"
  }
}
```

The helper is an executable you supply; its stdout must contain only the token.
Commands run directly, with no shell expansion, in the configuration file's
directory and inherit the proxy's environment. Use an explicit
`["sh", "-c", "..."]` when shell syntax is needed. Stdin and stderr are connected
to the null device. Command arguments and output are not included in errors or
logs. `cacheTTL` and `timeout` are positive Go duration strings (for example,
`30s` or `5m`); their defaults are five minutes and thirty seconds.

| Token | Environment | File | Command |
| --- | --- | --- | --- |
| Proxy authentication | Read at startup | Read at startup | Run once at startup |
| GitHub credential | Read at startup | Read on each selected request | Run on first selected request, then cache for the TTL |

Proxy authentication stays fixed until restart, including when a command's
`cacheTTL` expires. Failure to obtain it prevents startup. GitHub file and
command sources are read only after client authentication and route selection.
A command's TTL starts on successful completion. Concurrent requests for the
same credential share one invocation, while different credentials have separate
caches. Request cancellation stops that request's wait without canceling the
shared invocation, which has its own timeout. Caches are in memory for one proxy
process; separate `exec` invocations do not share them.

Failure to obtain a GitHub token returns `503 credential_unavailable` without
contacting GitHub. Expired tokens are not reused after a failed refresh; the next
request tries the source again. GitHub rejection never causes token refresh,
credential fallback, or request replay.

Files and command stdout are limited to 64 KiB including surrounding whitespace,
which is removed before validation. Empty tokens and internal whitespace or
control characters are rejected. Environment values must not contain surrounding
whitespace. Proxy authentication additionally rejects `:`, which separates a
credential hint. Equality between proxy and GitHub tokens is not rejected.

Configuration file paths are relative to the configuration file directory.
Omitting `server.listen` (or `server`) defaults to `127.0.0.1:8080`; `exec`
always uses an automatically assigned loopback port. If `server.ca` is supplied,
both `certificateFile` and `privateKeyFile` are required. Credentials remain an
array with unique IDs; `routes` is an ordered top-level array. Legacy `tokenEnv`,
`routing`, `caCertificate`, and `caPrivateKey` fields are no longer accepted.
The configuration version remains `1`.

The [example configuration](config.example.json) shows all three source forms;
replace its paths and helper command with your own sources.

## Temporary command sidecar

On Unix-like systems, `exec` starts a temporary proxy for one command:

```sh
./gh-router exec -config config.json -- gh repo view acme/main
```

The executor starts the proxy as a child sidecar, waits until it is ready, and
then replaces itself with the command. The command therefore keeps the
executor's PID, exit status, and signal behavior. The sidecar stops after its
parent command exits.

`exec` always listens on an automatically assigned `127.0.0.1` port; it does
not use `server.listen`. It replaces these variables in the command
environment:

```text
HTTPS_PROXY, HTTP_PROXY, https_proxy, http_proxy
NO_PROXY, no_proxy
SSL_CERT_FILE
GH_HOST
GH_TOKEN
```

`GH_TOKEN` contains the proxy access token, not a real GitHub token. To select
a configured credential hint explicitly, use:

```sh
./gh-router exec -config config.json -hint related -- gh api graphql ...
```

The executor removes configured token environment variables, known ambient GitHub
token variables, and variables equal to GitHub tokens loaded from the environment.
File and command GitHub sources are acquired lazily in the sidecar, so aliases of
those token values in arbitrary inherited environment variables cannot be detected
at launch. Their values are never added to the launched command's environment.

Proxy authentication is obtained once by the executor and sent to the sidecar
through a private inherited pipe, so a command source does not run twice. The
launched command receives the same authentication token through `GH_TOKEN`.

This mode is intended to wrap a trusted sandbox launcher. The sandbox must be
able to reach the host loopback proxy, read the temporary public CA file, and
receive the variables above, while preventing the untrusted command from
inspecting host processes or token sources. A command with unrestricted access
to same-user host processes can inspect or interfere with its sidecar and is
not isolated by this helper alone.

The helper does not set up Git authentication. In particular, it does not add
a Git `extraHeader` or credential helper. Configure Git separately when it is
needed.

## Sandbox environment

Download the public CA certificate from the proxy's HTTP endpoint, then
configure clients to trust it:

```sh
mkdir -p /run/gh-router
curl --fail --silent --show-error \
  --noproxy '*' \
  http://gh-router.internal:8080/ca.pem \
  --output /run/gh-router/ca.pem

export HTTPS_PROXY='http://gh-router.internal:8080'
export HTTP_PROXY="$HTTPS_PROXY"
export NO_PROXY=''

export SSL_CERT_FILE='/run/gh-router/ca.pem'
export GIT_SSL_CAINFO='/run/gh-router/ca.pem'

export GH_HOST='github.com'
export GH_TOKEN='a-separate-random-client-secret'

git config --global http.https://github.com/.extraHeader \
  "Authorization: Basic $(printf 'x-access-token:%s' "$GH_TOKEN" | base64 | tr -d '\n')"
```

`GET /ca.pem` uses the proxy listener itself and works for generated and
file-backed CAs. With an ephemeral CA, download it again after every proxy
restart. Plain HTTP cannot authenticate the certificate it returns, so this
bootstrap is intended for an isolated network where the proxy address and
traffic to it are trusted. Use an authenticated distribution channel or verify
the certificate out of band when that assumption does not hold.

Use HTTPS Git remotes, for example:

```sh
git clone https://github.com/acme/main.git
gh repo view acme/main
```

The proxy access token alone uses automatic routing. For a GraphQL operation
whose target cannot be inferred, append one of the non-secret hints configured
in `credentials[].hints`:

```sh
GH_TOKEN='a-separate-random-client-secret:related' gh api graphql ...
```

Hints select a credential but do not grant access to the proxy. A request must
always include the correct base proxy access token. An absent or unknown base
token, unknown hint, Cookie, URL credential, unsupported host, or non-Git
request to `github.com` is rejected before a real token is attached.

## Current scope

- Temporary per-command sidecar execution on Unix-like systems
- In-memory ephemeral CA generation and `GET /ca.pem`, or a file-backed CA
- `api.github.com`: REST and GraphQL
- `github.com`: Git smart HTTP only
- Static Fine-grained PATs loaded from environment variables
- Ordered exact-repository and owner routes, with an optional final unconditional route
- One proxy access token with optional per-credential routing hints

Git SSH, LFS, release/CDN downloads, GitHub Apps, and proxy-side operation policies are not part of the MVP.
