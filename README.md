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

Copy [`config.example.json`](config.example.json), update the routes, and provide the real tokens only to the proxy process:

```sh
export GH_ROUTER_TOKEN_MAIN='github_pat_...'
export GH_ROUTER_TOKEN_RELATED='github_pat_...'
export GH_ROUTER_CLIENT_TOKEN='a-separate-random-client-secret'
./gh-router -config config.json
```

When `server.caCertificate` and `server.caPrivateKey` are both omitted, the
proxy generates an ephemeral CA in memory at startup. Its private key is never
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
  "caCertificate": "/etc/gh-router/ca.pem",
  "caPrivateKey": "/etc/gh-router/ca-key.pem"
}
```

Real GitHub token environment variable names are configured in
`credentials[].tokenEnv`. The separate proxy access token is configured in
`authentication.tokenEnv`; it must not be the same value as any GitHub token.
Token values are never read from the JSON file.

## Temporary command sidecar

On Linux and macOS, `exec` starts a temporary proxy for one command:

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

The executor removes the configured real-token variables and known ambient
GitHub token variables from the command environment. The sidecar writes only
its public CA certificate to the temporary path in `SSL_CERT_FILE`; an
ephemeral CA private key remains in sidecar memory and is never written to
disk. The public certificate and its temporary directory are removed when the
command exits.

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

- Temporary per-command sidecar execution on Linux and macOS
- In-memory ephemeral CA generation and `GET /ca.pem`, or a file-backed CA
- `api.github.com`: REST and GraphQL
- `github.com`: Git smart HTTP only
- Static Fine-grained PATs loaded from environment variables
- Ordered exact-repository and owner routes, with an optional final unconditional route
- One proxy access token with optional per-credential routing hints

Git SSH, LFS, release/CDN downloads, GitHub Apps, and proxy-side operation policies are not part of the MVP.
