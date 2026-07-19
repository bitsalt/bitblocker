# Traefik integration

Audience: operators wiring BitBlocker into an existing Traefik reverse proxy. For the daemon's install/config walkthrough see the [README](../README.md); for the full design see [bitblocker-spec.md](bitblocker-spec.md).

BitBlocker is not a Traefik plugin — it's a small standalone HTTP daemon that Traefik calls as a [`forwardAuth`](https://doc.traefik.io/traefik/middlewares/http/forwardauth/) middleware on every request. Traefik forwards the original request's method/headers to BitBlocker's `/check` endpoint; BitBlocker's response code decides whether the original request proceeds.

## Register the middleware

Point a `forwardAuth` middleware at the daemon's `/check` endpoint:

```yaml
# traefik/dynamic/bitblocker.yml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://127.0.0.1:8080/check"
        trustForwardHeader: true
```

`trustForwardHeader: true` tells Traefik to forward the `X-Forwarded-For` / `X-Real-IP` headers it already has from the real client connection — see § Header contract below for why this setting is load-bearing, not cosmetic.

## Attach it to a router

```yaml
http:
  routers:
    my-service:
      rule: "Host(`example.com`)"
      middlewares:
        - bitblocker
      service: my-service
```

Any router with the `bitblocker` middleware attached has every request checked before it reaches `my-service`.

Traefik must be able to reach BitBlocker's listen address. If BitBlocker runs on the host and Traefik runs in Docker, use `host.docker.internal` (or the Docker bridge IP) as the `forwardAuth.address` host. If both run as containers on the same Docker network, use BitBlocker's container/service name and set `listen.host: 0.0.0.0` in its config — the daemon's default `127.0.0.1` only accepts connections from inside its own network namespace.

## Request contract: how BitBlocker identifies the client

Verified against `internal/server/clientip.go`. BitBlocker extracts the client address in this order:

1. **`X-Real-IP`**, if present and parseable — used as-is.
2. Otherwise, the **rightmost** parseable entry of **`X-Forwarded-For`**.

The rightmost-XFF choice is deliberate, not an oversight. Under Traefik's `trustForwardHeader: true`, the *leftmost* entry of `X-Forwarded-For` is whatever the original client claims — an attacker connecting directly can set it to anything. The *rightmost* entry is the address of whatever proxy hop was actually adjacent to Traefik, which Traefik itself appends and a client cannot forge. If neither header carries a parseable address, extraction fails and the request is denied (see below) — there is currently no config knob to prefer leftmost-XFF for upstream-CDN topologies (tracked as an Open Question in the sprint file).

This means: **for the header contract to mean anything, Traefik must be the component appending/rewriting these headers from the real TCP peer it saw** — not passing through whatever an upstream client or CDN already set unchecked. If another reverse proxy or CDN sits in front of Traefik, make sure it's Traefik (or that trusted intermediary) that BitBlocker ultimately treats as the source of truth for the rightmost hop.

## Response contract

Verified against `internal/server/server.go`:

| BitBlocker response | Traefik behavior | Meaning |
|---|---|---|
| `200 OK` | Forwards the original request | Client IP is not blocked |
| Configured block status (`behavior.response_code`, default `403`) | Blocks the original request; Traefik returns this status to the client | Client IP is blocked, **or** BitBlocker could not make a decision |

Two cases both return the block status, and both are fail-closed by design:

- The client IP matched a blocked country.
- The blocklist is not yet populated (cold start with no cache and no successful fetch yet), or `X-Real-IP`/`X-Forwarded-For` carried no parseable address. Either way, BitBlocker cannot answer "allowed," so it answers "blocked."

The response body is always empty; BitBlocker never returns a body Traefik would show the client.

## `/healthz` as a health check

`GET /healthz` reports whether the daemon has a populated blocklist and is ready to make `/check` decisions — not just whether the process is alive:

| Status | Body | Meaning |
|---|---|---|
| `503 Service Unavailable` | `{"status":"empty"}` | Cold-starting: no disk cache loaded and no successful fetch yet |
| `200 OK` | `{"status":"ok"}` | A populated blocklist is active |

Use this as a container `HEALTHCHECK` or a Traefik health check on the service so nothing routes traffic assuming BitBlocker is ready before it actually is:

```dockerfile
# Dockerfile HEALTHCHECK — bitblocker runs distroless (no shell, no curl);
# if you build your own wrapper image with a shell available:
HEALTHCHECK --interval=10s --timeout=2s --start-period=30s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
```

The daemon's own published image is distroless (no shell, no `curl`/`wget`) — see the [README](../README.md) § Install for the image. If you need an in-container `HEALTHCHECK`, wrap the binary in your own thin image that includes a fetch tool, or perform the check externally (e.g. from the orchestrator/host) against the mapped port.

## Minimal end-to-end example

Traefik dynamic configuration:

```yaml
# traefik/dynamic/bitblocker.yml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://127.0.0.1:8080/check"
        trustForwardHeader: true
  routers:
    my-service:
      rule: "Host(`example.com`)"
      middlewares:
        - bitblocker
      service: my-service
  services:
    my-service:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:9000"
```

BitBlocker running alongside (binary + systemd, per the README):

```yaml
# config.yaml
listen:
  host: "127.0.0.1"
  port: 8080

block:
  countries:
    - CN
    - RU

sources:
  dbip:
    enabled: true

refresh:
  schedule: "0 3 * * *"
  timeout: 30s

behavior:
  response_code: 403
  startup_mode: "fail-closed"

cache:
  path: "/var/cache/bitblocker/dbip-country-lite.mmdb"
  max_age: 48h
```

Both processes bind to `127.0.0.1` here on the assumption Traefik and BitBlocker share a host network namespace (bare-metal/systemd, or a Docker Compose network where BitBlocker publishes no external port). Adjust `listen.host` and the `forwardAuth.address` host together if your topology differs (see the Docker networking note in the README).
