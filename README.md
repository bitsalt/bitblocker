# BitBlocker

A self-hosted Go daemon that drops inbound traffic from specified countries at the Traefik edge. Sits behind Traefik as a `forwardAuth` middleware, maintains an in-memory CIDR trie populated from DB-IP "IP-to-Country Lite" data (ADR 0003), and refreshes itself on a cron schedule. v1 ships country-based blocking with IPv4 + IPv6 support and a fail-closed security posture.

Intended audience: operators running small-scale self-hosted infrastructure who want to eliminate automated scanning traffic without paying for a WAF.

See [docs/bitblocker-spec.md](docs/bitblocker-spec.md) for the full design, [docs/traefik-integration.md](docs/traefik-integration.md) for the Traefik wiring walkthrough, and [docs/BitBlocker.md](docs/BitBlocker.md) for the sprint plan and decisions log.

## Status

**v1.0.** The daemon runs end-to-end:

- HTTP forwardAuth server exposing `GET /check` and `GET /healthz` (`internal/server`).
- Lock-free atomic blocklist swap (`internal/blocklist`, ADR 0001) — `/check` never observes a partial update.
- On-disk cache with a fail-closed cold start (`internal/diskcache`, ADR 0002): a recent snapshot serves ahead of the first network fetch, and a stale or corrupt snapshot is discarded rather than trusted.
- DB-IP "IP-to-Country Lite" fetcher, cron scheduler, and a bounded cold-start retry budget (`internal/fetcher`, `internal/scheduler`, ADR 0003) — keyless, no account, no license cost.
- Multi-stage Docker image, a systemd unit, and a GitHub Actions release workflow producing linux/amd64 + linux/arm64 binaries and a multi-arch container image.

v1.1 (planned) adds a Prometheus `/metrics` endpoint and CLI inspection subcommands. See [docs/BitBlocker.md](docs/BitBlocker.md) for the live task/milestone state.

## Install

Two supported install paths for a tagged release (`vX.Y.Z`):

### a) Prebuilt binaries

Each release tag's GitHub Release carries `bitblocker-linux-amd64`, `bitblocker-linux-arm64`, and a `checksums.txt` (sha256):

```bash
curl -LO https://github.com/bitsalt/bitblocker/releases/latest/download/bitblocker-linux-amd64
curl -LO https://github.com/bitsalt/bitblocker/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing
chmod +x bitblocker-linux-amd64
sudo mv bitblocker-linux-amd64 /usr/local/bin/bitblocker
```

### b) Docker image

Multi-arch image at **`ghcr.io/bitsalt/bitblocker`** (linux/amd64 + linux/arm64), tagged per release (`vX.Y.Z`, `X.Y`, `X`). The daemon reads its config from the path given by `--config` (default `/etc/bitblocker/config.yaml`); no config ships in the image.

```bash
docker run -d \
  --name bitblocker \
  -p 127.0.0.1:8080:8080 \
  -v /path/to/config.yaml:/etc/bitblocker/config.yaml:ro \
  -v bitblocker-cache:/var/cache/bitblocker \
  ghcr.io/bitsalt/bitblocker
```

The cache volume matters: without a persistent mount at `/var/cache/bitblocker`, a container recreate loses the disk-cache snapshot and the daemon cold-starts fail-closed until the next successful fetch. If Traefik reaches BitBlocker over a shared Docker network rather than a published port, set `listen.host` to `0.0.0.0` (or the container's network-facing interface) in the mounted config — the default `127.0.0.1` only accepts connections from inside the same network namespace.

## Configuration

A single YAML file drives the daemon. [config.example.yaml](config.example.yaml) is the canonical template — copy it to a working location and edit. Fields, as implemented in `internal/config`:

- **`listen.host` / `listen.port`** — the forwardAuth server's bind address. Default `127.0.0.1:8080`.
- **`block.countries`** — ISO 3166-1 alpha-2 codes to block (validated against `^[A-Z]{2}$`). At least one of `block.countries` or `block.asns` must be non-empty.
- **`block.asns`** — accepted by the schema; blocking is **not implemented in v1** (deferred — see the ASN-blocking Open Question in the sprint file).
- **`sources.dbip.enabled`** — the DB-IP "IP-to-Country Lite" source. No account, key, or edition to configure — the download URL is derived at fetch time (ADR 0003). Default `true`.
- **`sources.bgptools.enabled`** — accepted by the schema; not implemented in v1. At least one of `sources.dbip.enabled` / `sources.bgptools.enabled` must be `true`.
- **`refresh.schedule`** — a cron expression governing how often the blocklist refreshes. Default `0 3 * * *` (daily, 3am). DB-IP only publishes monthly; a daily cron against a monthly file is deliberate (ADR 0003) — nearly every check is a cheap conditional-GET 304.
- **`refresh.timeout`** — per-fetch timeout. Default `30s`.
- **`behavior.log_blocked`** / **`behavior.log_allowed`** — whether `/check` emits an INFO line on a blocked decision or a DEBUG line on an allowed one. Fail-closed denials are always logged at WARN regardless of these flags. Defaults `true` / `false`.
- **`behavior.response_code`** — the HTTP status `/check` returns for a blocked or fail-closed request. Must be a 4xx/5xx status. Default `403`.
- **`behavior.startup_mode`** — `fail-closed` | `fail-open`. Default `fail-closed`. The field is validated and logged at startup. **Verification note:** at the time of this v1.0 pass, no code path in `internal/server`, `internal/fetcher`, or `internal/scheduler` was found branching on this value — `/check` and `/healthz` fail closed on an empty blocklist regardless of the configured mode. Treat `fail-open` as reserved/not-yet-wired until confirmed otherwise by Developer/Architect; this README will be corrected once verified.
- **`cache.path`** — the on-disk blocklist snapshot path. Default `/var/cache/bitblocker/dbip-country-lite.mmdb`.
- **`cache.max_age`** — how old a snapshot may be before it's rejected as stale (and removed) rather than trusted at cold start. Default `48h`.
- **`logging.level`** — `debug` | `info` | `warn` | `error`. Default `info`.
- **`logging.format`** — `json` | `text`. Default `json`.

## Running under systemd

[packaging/systemd/bitblocker.service](packaging/systemd/bitblocker.service) is the reference unit. Install the binary at `/usr/local/bin/bitblocker`, supply `/etc/bitblocker/config.yaml` yourself (the unit does not provision it), and enable the unit:

```bash
sudo cp packaging/systemd/bitblocker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now bitblocker
```

`DynamicUser=true` creates a transient, unprivileged `bitblocker` user at start — no manual `useradd` step. `CacheDirectory=bitblocker` creates `/var/cache/bitblocker` (mode `0750`, owned by that dynamic user) before `ExecStart` runs, and — unlike a `RuntimeDirectory` — persists it across restarts, which the disk-cache cold-start path depends on.

## Repository layout

```
cmd/bitblocker/          # daemon entry point
internal/
  blocklist/             # CIDR trie, atomic-swap Source (ADR 0001)
  config/                # YAML schema, Load, Validate
  diskcache/             # on-disk blocklist snapshot, fail-closed cold start (ADR 0002)
  fetcher/               # DB-IP fetcher: conditional GET, month-rollover fallback, gunzip
  logging/                # log/slog setup, context helpers, redaction
  mmdb/                   # MMDB decode (country.iso_code only) — DB-IP-compatible (ADR 0003)
  scheduler/             # cron-driven periodic refresh + bounded cold-start retry
  server/                # forwardAuth HTTP server (/check, /healthz)
packaging/systemd/       # bitblocker.service reference unit
scripts/git-hooks/       # pre-commit hook
.github/workflows/       # CI + release (binaries + Docker image)
docs/                    # spec, sprint plan, decisions, ADRs, Traefik integration
config.example.yaml
Dockerfile
.dockerignore
Makefile
NOTICE                   # DB-IP CC-BY 4.0 attribution
```

## Build and test

Requires Go 1.25.12 or newer.

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

## License / attribution

BitBlocker's code is MIT — see [LICENSE](LICENSE). Reasoning is in the decisions log in [docs/BitBlocker.md](docs/BitBlocker.md).

The country GeoIP data is **IP geolocation data by DB-IP (https://db-ip.com), licensed [CC-BY 4.0](https://creativecommons.org/licenses/by/4.0/)**. See [NOTICE](NOTICE) for the full attribution. CC-BY covers the data, not the software — there is no license-compatibility conflict for an edge daemon that bundles no data in its binary and downloads it at runtime (ADR 0003 § Licensing/attribution).
