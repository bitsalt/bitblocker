# BitBlocker

A self-hosted Go daemon that drops inbound traffic from specified countries at the Traefik edge. Sits behind Traefik as a `forwardAuth` middleware, maintains an in-memory CIDR trie populated from MaxMind GeoLite2 data, and refreshes itself on a cron schedule. v1 targets country-based blocking with IPv4 + IPv6 support and a fail-closed security posture.

Intended audience: operators running small-scale self-hosted infrastructure who want to eliminate automated scanning traffic without paying for a WAF.

See [docs/bitblocker-spec.md](docs/bitblocker-spec.md) for the full design and [docs/BitBlocker.md](docs/BitBlocker.md) for the sprint plan and decisions log.

## Status

**Sprint 1 complete — foundation.** The repository has a working Go module, CI, structured logging, a validated config schema, and a benchmarked CIDR trie. The daemon does not yet run end-to-end; Sprint 2 wires the HTTP server and atomic blocklist swap, Sprint 3 adds the fetcher and scheduler.

| Component | State |
|---|---|
| Repo scaffold (`cmd/`, `internal/`) | ✅ |
| CI (build, vet, race tests, govulncheck, golangci-lint) | ✅ |
| `internal/config` — typed YAML schema + validation | ✅ |
| `internal/logging` — `log/slog` JSON handlers + `Redact()` | ✅ |
| `internal/blocklist` — IPv4/IPv6 CIDR trie | ✅ |
| `internal/server` — forwardAuth HTTP endpoints | ⬜ Sprint 2 |
| `internal/fetcher` — MaxMind GeoLite2 fetcher | ⬜ Sprint 3 |
| v1.0 release (binaries + Docker + docs) | ⬜ Sprint 4 |

## Repository layout

```
cmd/bitblocker/          # daemon entry point
internal/
  blocklist/             # CIDR trie, lookup logic
  config/                # YAML schema, Load, Validate
  fetcher/               # upstream data fetchers (skeleton)
  logging/               # log/slog setup, context helpers, redaction
  server/                # forwardAuth HTTP server (skeleton)
scripts/git-hooks/       # pre-commit hook
.github/workflows/       # CI
docs/                    # spec, sprint plan, decisions
config.example.yaml
Makefile
```

## Build and test

Requires Go 1.22 or newer.

```bash
go build ./cmd/bitblocker     # or: make build
go test -race ./...           # or: make test
golangci-lint run             # or: make lint
govulncheck ./...             # or: make vuln
```

`make` wraps each of the above; if `make` is not installed on your system, run the `go …` commands directly — they are equivalent.

## Contributor setup

Activate the pre-commit hook once per clone:

```bash
git config core.hooksPath scripts/git-hooks
```

The hook runs `golangci-lint fmt` and `golangci-lint run` on every commit. Install golangci-lint if you do not have it:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

CI pins `golangci-lint v2.11.4`. Local versions slightly ahead or behind are fine for day-to-day work; when CI diverges from local, bump the pinned version in [.github/workflows/ci.yml](.github/workflows/ci.yml).

## Configuration

A single YAML file drives the daemon. [config.example.yaml](config.example.yaml) is the canonical template — copy it to a working location and edit.

The MaxMind license key may be supplied inline or via the `MAXMIND_LICENSE_KEY` environment variable. The environment variable wins if both are set; prefer it for anything beyond a local experiment so the secret does not sit on disk.

## License

TBD before the v1.0 tag (MIT is the default assumption — see the open questions in [docs/BitBlocker.md](docs/BitBlocker.md)).
