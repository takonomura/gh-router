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

The token environment variable names are configured in `credentials[].tokenEnv`. Token values are never read from the JSON file.

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
export GH_TOKEN='gh-router-auto'
export GH_PROMPT_DISABLED='1'
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

For a GraphQL operation whose target cannot be inferred, select a configured non-secret hint:

```sh
GH_TOKEN='gh-router-related' gh api graphql ...
```

An unknown `Authorization` value, Cookie, URL credential, unsupported host, or non-Git request to `github.com` is rejected before a real token is attached.

## Current scope

- In-memory ephemeral CA generation and `GET /ca.pem`, or a file-backed CA
- `api.github.com`: REST and GraphQL
- `github.com`: Git smart HTTP only
- Static Fine-grained PATs loaded from environment variables
- Exact repository routes, owner routes, credential hints, and an optional explicit default

Git SSH, LFS, release/CDN downloads, GitHub Apps, and proxy-side operation policies are not part of the MVP.
