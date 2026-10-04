# gh-router design

## Purpose

The intended use is a development sandbox that writes to a main repository and
reads related private repositories owned by other organizations, plus public
repositories. The host holds a Fine-grained PAT for the main repository and
read-only PATs for the related owners. The sandbox holds only a proxy access token.

Routing selects a credential; GitHub enforces that credential's repository access
and permissions. This avoids maintaining a second authorization model for API
operations or Git refs. A proxy instance may be shared only by sandboxes allowed
to use the same credential set.

See the [README](../README.md) for configuration and client setup.

## Request flow

The proxy terminates client TLS so it can authenticate the client, inspect the
request target, and replace its authorization header. It opens a separate,
certificate-verified TLS connection to GitHub.

1. Validate the destination host and request identity.
2. Authenticate the proxy access token and reject other credential carriers.
3. Select one credential using an explicit hint or the request's owner/repository.
4. Obtain the selected GitHub token and construct a new authorization header:
   Bearer for API requests, Basic for Git.
5. Forward the request once and return the upstream response.

Supported destinations are fixed to `api.github.com` for REST/GraphQL and
`github.com` for Git smart HTTP. This keeps the token injection boundary explicit;
configuration cannot add arbitrary destinations. There is no per-operation API
allowlist. Configuration uses JSON and the implementation uses Go's standard
library to keep dependencies small.

## Credential selection

Clients send the proxy access token as a Bearer/token value or Basic password.
Appending `:hint` selects the credential associated with that configured hint.
A hint takes precedence over target extraction, but still requires valid client
authentication and a supported host/protocol. GitHub decides whether the chosen
credential can access the requested target.

Without a hint, the proxy extracts targets from these request forms:

| Protocol | Target extraction |
| --- | --- |
| REST | Repository from `/repos/{owner}/{repo}/...`; owner from `/orgs/{owner}/...` or `/users/{owner}/...`; `repo:`, `org:`, or `user:` qualifiers from `q` on REST search endpoints |
| GraphQL | Recognized owner/repository variables and repository, organization, or user literals in the JSON body; search qualifiers from `search(query: ...)` literals or referenced JSON variables |
| Git | Repository from smart HTTP discovery, upload-pack, and receive-pack paths |

GraphQL and search extraction are best-effort, not complete document or search
expression parsers. Requests such as node-ID-only mutations may need a hint or
an unconditional route.

For each extracted target, routes are evaluated in configuration order with
case-insensitive owner/repository matching. There is no implicit preference for
repository routes over owner routes. An optional final unconditional route
matches any target, including requests with no extracted target.

Every target must resolve to the same credential. No match produces
`route_not_found`; targets resolving to different credentials produce
`ambiguous_route`. Malformed targets are rejected rather than sent to the
unconditional route. Requests are never split across credentials or replayed
with another token after GitHub rejects them.

## Security boundaries

Real tokens are attached only after client authentication, destination validation,
and credential selection. The proxy rejects unknown client credentials, Cookies,
URL userinfo, and token-bearing query parameters. Authorization is rebuilt from
the selected token, and real tokens are excluded from logs and errors.

CONNECT is limited to supported hosts on port 443. CONNECT authority, TLS SNI,
and HTTP Host must agree. Requests to `github.com` must use supported Git smart
HTTP paths. The only plain HTTP endpoint is the proxy's public `GET /ca.pem`.
Upstream certificates are verified and redirects are not followed by the proxy.

The CA private key stays on the host. Downloading its public certificate over
HTTP requires a trusted proxy address and network; otherwise an authenticated
distribution or verification channel is necessary.

All authenticated clients can select every configured hint. Token permissions
must therefore be safe for every sandbox using the instance. Routing does not
restrict an overly broad PAT, prevent secrets being written to an allowed
repository, or provide different permissions for different clients.

The sandbox must independently block access to host processes and token sources,
and egress controls must prevent bypassing the proxy. The temporary `exec` proxy
helps launch a configured sandbox but does not create this isolation. Its
environment filtering also cannot detect arbitrary inherited copies of tokens
obtained lazily from files or commands. CONNECT and TLS setup precede client
authentication, so the proxy is intended for an isolated network and does not
provide network-level denial-of-service protection.
