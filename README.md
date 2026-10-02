# gh-router

`gh-router` is an HTTPS forward proxy that keeps real GitHub tokens outside a
sandbox and selects a credential for each `gh` or Git request. Clients send a
separate proxy access token; the proxy replaces it with the selected GitHub token.
GitHub token permissions determine what the client can do.

It supports REST and GraphQL on `api.github.com` and Git smart HTTP on
`github.com`. SSH, Git LFS, release/CDN transfers, and GitHub App token generation
are outside the current scope. See [the design](docs/design.md) for the routing
model and security boundaries.

## Setup

Build with Go 1.24 or newer. No third-party dependencies are required.

```sh
go build -o gh-router ./cmd/gh-router
```

On the host, save this as `config.json`, replacing the repository and owner names:

```json
{
  "version": 1,
  "server": {"listen": "127.0.0.1:8080"},
  "authentication": {
    "tokenFrom": {"env": "GH_ROUTER_CLIENT_TOKEN"}
  },
  "routes": [
    {"when": {"repository": "acme/main"}, "credential": "main"},
    {"when": {"owner": "partner"}, "credential": "partner-read"},
    {"credential": "main"}
  ],
  "credentials": [
    {
      "id": "main",
      "tokenFrom": {"env": "GH_ROUTER_TOKEN_MAIN"},
      "hints": ["main"]
    },
    {
      "id": "partner-read",
      "tokenFrom": {"env": "GH_ROUTER_TOKEN_PARTNER"},
      "hints": ["partner"]
    }
  ]
}
```

Give `main` only the repository access and write permissions it needs, and
`partner-read` read-only access to the required partner repositories. Set the
real tokens only in the host proxy environment, then start the proxy:

```sh
export GH_ROUTER_TOKEN_MAIN='github_pat_...'
export GH_ROUTER_TOKEN_PARTNER='github_pat_...'
export GH_ROUTER_CLIENT_TOKEN='a-separate-random-client-secret'
./gh-router serve -config config.json
```

### Configure the sandbox

Use a proxy address reachable from the sandbox. The default loopback listener
works only where the sandbox can reach the host loopback; otherwise configure
`server.listen` and the sandbox network accordingly. In this example,
`gh-router.internal:8080` is that reachable address.

Run the following in the sandbox, using the same proxy access token as the host:

```sh
mkdir -p /run/gh-router
curl --fail --silent --show-error --noproxy '*' \
  http://gh-router.internal:8080/ca.pem --output /run/gh-router/ca.pem

export HTTPS_PROXY='http://gh-router.internal:8080'
export HTTP_PROXY="$HTTPS_PROXY"
export NO_PROXY=''
export SSL_CERT_FILE='/run/gh-router/ca.pem'
export GIT_SSL_CAINFO='/run/gh-router/ca.pem'
export GH_HOST='github.com'
export GH_TOKEN='a-separate-random-client-secret'

git config --global http.https://github.com/.extraHeader \
  "Authorization: Basic $(printf 'x-access-token:%s' "$GH_TOKEN" | base64 | tr -d '\n')"

gh repo view acme/main
git clone https://github.com/acme/main.git
```

By default, the proxy generates an in-memory CA valid for 24 hours. Download its
public certificate again after restarting the proxy. The HTTP download assumes
a trusted proxy address and network; otherwise distribute or verify the
certificate through an authenticated channel. Keep the CA private key on the host.

### Run a temporary proxy

On Unix-like systems, `exec` runs a proxy for the lifetime of one command:

```sh
./gh-router exec -config config.json -- gh repo view acme/main
```

It uses an automatically assigned loopback port, sets the proxy variables,
clears `NO_PROXY`, and supplies `SSL_CERT_FILE`, `GH_HOST`, and `GH_TOKEN`.
Configured token environment variables and known ambient GitHub token variables
are removed from the command environment.

For isolation, wrap a trusted sandbox launcher. It must pass these settings into
the sandbox, make the host loopback proxy and temporary public CA file available,
and prevent access to host processes and token sources. `exec` alone does not
isolate an untrusted command. Git still needs the authentication header and
`GIT_SSL_CAINFO` setup shown above, using the values supplied by `exec`.

## Configuration

Routes are evaluated in order; the first match wins. Use `when.repository` for
an exact repository or `when.owner` for an owner. Names are case-insensitive.
An optional unconditional route must be last; it also handles requests whose
target cannot be extracted. With no matching route, the request is rejected.

A hint explicitly selects a credential, for example when a GraphQL operation
contains only node IDs. Hints are the non-secret names in `credentials[].hints`:

```sh
GH_TOKEN='a-separate-random-client-secret:partner' gh api graphql \
  -f query='{ viewer { login } }'

./gh-router exec -config config.json -hint partner -- gh api graphql \
  -f query='{ viewer { login } }'
```

Both `authentication.tokenFrom` and `credentials[].tokenFrom` accept exactly one
source. [config.example.json](config.example.json) shows all three forms.

| Source | GitHub credential behavior |
| --- | --- |
| `env` | Read at startup |
| `file` | Read on each request that selects the credential |
| `command` | Run on first selection, then cache for `cacheTTL` |

Proxy authentication is always read once at startup and fixed until restart.
Command helpers receive an `argv` array, run without shell expansion in the
configuration directory, and must print only the token to stdout. The default
`cacheTTL` is `5m` and `timeout` is `30s`; both accept positive Go durations.
Keep the cache TTL shorter than the token's lifetime. GitHub token acquisition
failure returns `503 credential_unavailable`; expired cached tokens are not reused.

Relative file paths are resolved from the configuration directory. Omitting
`server.listen` defaults to `127.0.0.1:8080`. To use an externally managed CA,
set both `server.ca.certificateFile` and `server.ca.privateKeyFile`.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/gh-router
```

Tests use a local fake upstream and need no real GitHub token. With `gh` and
`git` installed, require the complete command E2E suite:

```sh
GH_ROUTER_REQUIRE_GH_E2E=1 go test ./...
```
