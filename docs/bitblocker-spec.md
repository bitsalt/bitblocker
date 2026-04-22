# BitBlocker — project spec

## Overview

BitBlocker is a self-hosted, lightweight Go daemon that blocks inbound traffic from specified countries or autonomous systems (ASNs). It maintains a locally cached blocklist built from live BGP/GeoIP data sources, applies rules at the appropriate enforcement layer, and refreshes itself on a configurable schedule.

The initial target integration is Traefik via its `forwardAuth` middleware. Additional enforcement backends (iptables/nftables, Nginx, Caddy) are scoped to v2.

The guiding design principle: **drop silently**. Blocked requests get no response body, no redirect, no indication that a service exists. Attackers get a timeout.

---

## Problem statement

Self-hosted infrastructure — particularly servers running WordPress, Laravel, or similar stacks — attracts automated scanning traffic originating from cloud infrastructure in specific regions. This traffic probes known vulnerability paths (xmlrpc.php, wp-login.php, common CVE endpoints) at scale. The goal is to eliminate this noise before it reaches application logic, without relying on third-party WAFs or paid services.

Existing tools solve parts of this problem but not all of it:

- `geoipupdate` refreshes GeoIP data but doesn't apply rules
- `fail2ban` is reactive, not proactive
- Manual CIDR lists go stale and are painful to manage
- No purpose-built tool wraps data freshness, rule generation, and Traefik awareness in a single self-hosted daemon

---

## Goals

- Block traffic by country code and/or ASN
- Pull blocklist data from free, reliable sources and keep it fresh automatically
- Expose a `forwardAuth`-compatible HTTP endpoint for Traefik integration
- Apply a true DROP behavior — no response to blocked clients
- Run as a single binary with a single config file
- Be safe to run on a single-droplet self-hosted setup without meaningful overhead

## Non-goals (v1)

- iptables/nftables direct rule injection (v2)
- Nginx or Caddy plugin support (v2)
- Multi-node or distributed operation
- Paid data source integrations (MaxMind GeoIP2 commercial)
- Web dashboard or management UI
- IPv6 support (deferred — evaluated at implementation time)

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    BitBlocker daemon                │
│                                                     │
│  ┌──────────────┐    ┌──────────────────────────┐  │
│  │ Data fetcher │───▶│ Blocklist cache (in-mem) │  │
│  │ (scheduled)  │    │ CIDR trie / ipset struct  │  │
│  └──────────────┘    └────────────┬─────────────┘  │
│                                   │                 │
│                      ┌────────────▼─────────────┐  │
│                      │  forwardAuth HTTP server  │  │
│                      │  GET /check               │  │
│                      └──────────────────────────┘  │
└─────────────────────────────────────────────────────┘
         ▲
         │ forwardAuth middleware
┌────────┴──────┐
│    Traefik    │
└───────────────┘
```

### Components

**Data fetcher**
Responsible for pulling CIDR data from configured sources, parsing it, and populating the in-memory blocklist. Runs at startup and on a configurable schedule (default: 24h).

**Blocklist cache**
An in-memory IP prefix trie (or similar structure suitable for fast CIDR lookups). Populated by the data fetcher. Lookups must be fast enough to add negligible latency to proxied requests — target under 1ms per check.

**forwardAuth HTTP server**
A minimal HTTP server exposing a single endpoint. Traefik calls this for every incoming request, passing the client IP via `X-Forwarded-For` or `X-Real-IP`. The server responds:

- `200 OK` — request is allowed, Traefik forwards it
- `403 Forbidden` with an empty body — request is blocked

Note: Traefik's forwardAuth does not support true TCP DROP. The `403` with no body is the closest practical equivalent at the application layer. True DROP requires iptables/nftables (v2).

---

## Data sources

### v1 sources

| Source | Type | License | Notes |
|---|---|---|---|
| [BGP.tools](https://bgp.tools) | ASN → CIDR | Free, attribution | Bulk download available |
| [MaxMind GeoLite2](https://dev.maxmind.com/geoip/geolite2-free-geolocation-data) | Country → CIDR | Free, registration required | Well-maintained, widely used |
| [Routeviews](http://www.routeviews.org) | BGP routing table | Free | Raw BGP data, requires parsing |

MaxMind GeoLite2 is the recommended default. It requires a free account and license key, which fits the self-hosted audience without adding cost.

BGP.tools ASN data is the recommended complement for ASN-level blocking.

### Blocklist update strategy

- Pull on daemon startup
- Refresh on a cron-style schedule (configurable, default `0 3 * * *` — 3am daily)
- On fetch failure, retain the existing in-memory blocklist and log a warning
- Do not apply a partial or corrupted update — atomic swap only

---

## Configuration

Single YAML file, path configurable via flag (default: `/etc/bitblocker/config.yaml`).

```yaml
listen:
  host: "127.0.0.1"
  port: 8080

block:
  countries:
    - CN
    - RU
  asns:
    - 4134   # China Telecom
    - 4837   # China Unicom
    - 9808   # China Mobile
    - 45090  # Tencent Cloud
    - 37963  # Alibaba Cloud

sources:
  maxmind:
    enabled: true
    license_key: "YOUR_LICENSE_KEY"
    edition: "GeoLite2-Country"
  bgptools:
    enabled: true

refresh:
  schedule: "0 3 * * *"
  timeout: "30s"

behavior:
  log_blocked: true
  log_allowed: false
  response_code: 403  # used for forwardAuth; DROP requires v2 iptables backend

logging:
  level: "info"   # debug | info | warn | error
  format: "json"  # json | text
```

---

## HTTP API

### `GET /check`

Called by Traefik's forwardAuth middleware. BitBlocker reads the client IP from the `X-Forwarded-For` header (first IP) or `X-Real-IP`, checks it against the blocklist, and responds accordingly.

**Headers expected:**
- `X-Forwarded-For: <client-ip>[, <proxy-ip>...]`
- `X-Real-IP: <client-ip>` (fallback)

**Responses:**

| Code | Meaning |
|---|---|
| `200 OK` | IP is not blocked; Traefik forwards the request |
| `403 Forbidden` | IP is blocked; Traefik rejects the request |

Body is always empty.

### `GET /healthz`

Returns `200 OK` with `{"status":"ok"}` if the daemon is running and the blocklist is populated. Returns `503` if the blocklist is empty (e.g., initial fetch has not completed or failed).

### `GET /metrics` *(optional, v1)*

Prometheus-compatible metrics endpoint. Counters for requests checked, requests blocked, blocklist size, last refresh timestamp, last refresh duration.

---

## Traefik integration

### Middleware configuration

```yaml
# traefik/dynamic/bitblocker.yml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://127.0.0.1:8080/check"
        trustForwardHeader: true
```

### Apply to a router

```yaml
http:
  routers:
    my-service:
      rule: "Host(`example.com`)"
      middlewares:
        - bitblocker
      service: my-service
```

Traefik must be able to reach BitBlocker's listen address. If BitBlocker runs on the host and Traefik in Docker, use `host.docker.internal` or bind to the Docker bridge IP.

---

## Deployment

### Binary

Build a single static binary. Distribute via GitHub Releases. Supports linux/amd64 and linux/arm64.

```
bitblocker -config /etc/bitblocker/config.yaml
```

### Systemd unit

```ini
[Unit]
Description=BitBlocker geo/ASN block daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/bitblocker -config /etc/bitblocker/config.yaml
Restart=on-failure
RestartSec=5s
User=bitblocker
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
```

### Docker

Provide an official `Dockerfile` and a `docker-compose` snippet. Config and license key injected via volume and environment variable respectively.

```yaml
services:
  bitblocker:
    image: ghcr.io/bitsalt/bitblocker:latest
    restart: unless-stopped
    volumes:
      - ./bitblocker.yaml:/etc/bitblocker/config.yaml:ro
    environment:
      - MAXMIND_LICENSE_KEY=${MAXMIND_LICENSE_KEY}
    ports:
      - "127.0.0.1:8080:8080"
```

---

## Phased roadmap

### v1 — Traefik forwardAuth daemon

- Go daemon with in-memory CIDR trie
- MaxMind GeoLite2 country blocking
- BGP.tools ASN blocking
- `forwardAuth` HTTP endpoint
- YAML config
- Systemd unit + Docker image
- `/healthz` and basic structured logging
- GitHub Actions CI: build, test, release binaries

### v1.1 — Observability and ops

- `/metrics` Prometheus endpoint
- Alerting hooks (webhook on refresh failure)
- CLI subcommand: `bitblocker check <ip>` — test an IP against the current blocklist without starting the daemon
- CLI subcommand: `bitblocker list` — print current blocklist stats (CIDR count by country/ASN, last refresh time)

### v2 — Additional enforcement backends

- **iptables/nftables backend:** generates and applies DROP rules directly, eliminating the Traefik dependency for use cases where kernel-level dropping is preferred
- **ipset integration:** loads CIDRs into an ipset table for high-performance kernel lookups
- **Nginx auth_request support:** equivalent to Traefik's forwardAuth for Nginx users
- **Caddy module:** native Caddy plugin

### v3 — Extended intelligence

- Threat feed integration (AbuseIPDB, Spamhaus)
- Dynamic banning based on request pattern matching (reactive layer, complements proactive geo/ASN blocking)
- Optional lightweight web UI for blocklist visibility and manual overrides

---

## Open questions

- **MaxMind license key requirement** — GeoLite2 requires registration. Worth evaluating `ip-api.com` or `DB-IP` free-tier databases as no-registration alternatives, even if less accurate. Decision needed before v1 release.
- **IPv6** — GeoLite2 includes IPv6 ranges. The CIDR trie implementation should be evaluated for IPv6 support early to avoid a painful retrofit.
- **Header trust** — `X-Forwarded-For` can be spoofed if BitBlocker is reachable directly. The listen address must be `127.0.0.1` or a private network interface. Document this clearly.
- **License** — MIT is the default assumption. Confirm before publishing.

---

## Repository structure (proposed)

```
bitblocker/
├── cmd/
│   └── bitblocker/
│       └── main.go
├── internal/
│   ├── blocklist/      # CIDR trie, lookup logic
│   ├── fetcher/        # MaxMind, BGP.tools data fetchers
│   ├── server/         # HTTP server, handler logic
│   └── config/         # YAML parsing, validation
├── deploy/
│   ├── bitblocker.service
│   └── docker-compose.yml
├── docs/
│   └── traefik-integration.md
├── config.example.yaml
├── Dockerfile
├── Makefile
└── README.md
```
