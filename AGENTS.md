# Development guidelines

## Priorities

- Build the smallest implementation that covers a concrete current use case.
- Prefer the Go standard library. Add a dependency only when a small, maintainable implementation would not be reasonable.
- Do not generalize for hypothetical protocols, hosts, policy models, or edge cases. Keep extension points local so support can be added after a real need appears.
- Treat GitHub token scopes as the MVP authorization boundary. Routing chooses a credential; it is not a second policy engine.

## Security requirements

- Never forward an unknown client credential or log a real GitHub token.
- Inject real tokens only into explicitly supported GitHub requests.
- Fail closed when a route is ambiguous, configuration is invalid, or request identity cannot be trusted.
- Do not retry a request with another credential after GitHub rejects it.

## Working practice

- Re-read `docs/design.md` before expanding behavior.
- Regularly ask whether code, abstraction, or dependency can be removed without losing an accepted use case.
- Cover the main routing and credential-isolation paths with tests; add edge-case handling when it protects a realistic security boundary or fixes observed behavior.
- Keep this file concise. Put detailed protocol or design notes in `docs/` and link them instead of growing this file indefinitely.

## Repository map

- `cmd/gh-router`: CLI entry point.
- `internal/ghrouter`: JSON configuration, credential routing, and proxy implementation.
- `config.example.json`: supported MVP configuration shape.
- Verify changes with `go test ./...`, `go vet ./...`, and `go build ./cmd/gh-router`.
