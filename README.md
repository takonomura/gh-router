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

Generate a CA used only by the sandbox and this proxy:

```sh
openssl req -x509 -newkey rsa:3072 -nodes -days 30 \
  -keyout ca-key.pem \
  -out ca.pem \
  -subj '/CN=gh-router local CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
chmod 600 ca-key.pem
```

Copy [`config.example.json`](config.example.json), update the routes and CA paths, then provide the real tokens only to the proxy process:

```sh
export GH_ROUTER_TOKEN_MAIN='github_pat_...'
export GH_ROUTER_TOKEN_RELATED='github_pat_...'
./gh-router -config config.json
```

The token environment variable names are configured in `credentials[].tokenEnv`. Token values are never read from the JSON file.

## Sandbox environment

Install or mount `ca.pem` in the sandbox, but never expose `ca-key.pem` or the real GitHub tokens. Configure clients as follows:

```sh
export HTTPS_PROXY='http://gh-router.internal:8080'
export HTTP_PROXY="$HTTPS_PROXY"
export NO_PROXY=''

export SSL_CERT_FILE='/run/gh-router/ca.pem'
export GIT_SSL_CAINFO='/run/gh-router/ca.pem'

export GH_HOST='github.com'
export GH_TOKEN='gh-router-auto'
export GH_PROMPT_DISABLED='1'
```

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

- `api.github.com`: REST and GraphQL
- `github.com`: Git smart HTTP only
- Static Fine-grained PATs loaded from environment variables
- Exact repository routes, owner routes, credential hints, and an optional explicit default

Git SSH, LFS, release/CDN downloads, GitHub Apps, and proxy-side operation policies are not part of the MVP.
