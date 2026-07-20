# Deployment guide

Audience: an operator or a DevOps agent standing BitBlocker up in
production. This is the deployment reference — it is also the intended
single input for a forthcoming ansible role, so every claim below is
grounded in a shipped artifact rather than restated from memory.

BitBlocker is a shipped v1.0 tool but is **not yet deployed anywhere in the
BitSalt estate** — no ansible role, no host, nothing in the fleet currently
references it. This guide exists to change that.

For the daemon's design, HTTP API, and data sources see
[bitblocker-spec.md](bitblocker-spec.md). For the Traefik `forwardAuth`
wiring and header contract, see
[traefik-integration.md](traefik-integration.md) — this guide links to it
rather than duplicating it. For install/config detail beyond what's
essential here, see the [README](../README.md).

## Image

The multi-arch image is published at **`ghcr.io/bitsalt/bitblocker`**
(`linux/amd64` + `linux/arm64`), public and anonymously pullable — verified
at the v1.0.0 release (docs/BitBlocker.md decisions log, 2026-07-20: the
`:latest` manifest returned HTTP 200 for both platforms with no
authentication). Each release publishes four tags derived from the git tag:
`X.Y.Z`, `X.Y`, `X`, and `latest` (e.g. a `v1.0.0` git tag produces image
tags `1.0.0`, `1.0`, `1`, `latest` — no `v` prefix on the image tag).

**Pin a specific tag (`:1.0.0` or `:1.0`) for reproducible deploys — do not
deploy `:latest`.** This is the single most important thing to get right
when wiring up a deployment: `:latest` will silently move out from under a
running deployment on the next release, and a pinned tag is the only way to
control when BitBlocker actually upgrades.

## Run mode (a): container

A representative `docker run`, matching the shipped `Dockerfile`'s runtime
contract (distroless, non-root, reads `--config` from
`/etc/bitblocker/config.yaml` by default, no config baked into the image):

```bash
docker run -d \
  --name bitblocker \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v /etc/bitblocker/config.yaml:/etc/bitblocker/config.yaml:ro \
  -v bitblocker-cache:/var/cache/bitblocker \
  ghcr.io/bitsalt/bitblocker:1.0.0
```

Two mounts matter:

- The config bind mount. No config ships in the image; the daemon fails to
  start without one at the path given by `--config`.
- **The cache volume, mounted persistently at `/var/cache/bitblocker`.**
  The `Dockerfile` declares this a `VOLUME`, so an anonymous volume is
  created even if you omit `-v`, but an anonymous volume does not survive a
  `docker rm`/recreate. Without a **named or bind-mounted** volume here, a
  container recreate loses the on-disk blocklist snapshot and the daemon
  cold-starts fail-closed (denying everything) until the next successful
  fetch. Use a named volume (as above) or a host bind mount — either
  survives recreation; an anonymous volume does not.

If Traefik reaches BitBlocker over a shared Docker network rather than a
published port, set `listen.host: 0.0.0.0` in the mounted config instead of
publishing the port — see § Client-IP trust below for why binding on a
publicly reachable interface is the thing to avoid regardless of which of
these two you choose.

## Run mode (b): systemd on the host

The reference unit is
[`packaging/systemd/bitblocker.service`](../packaging/systemd/bitblocker.service).
Install the binary at `/usr/local/bin/bitblocker`, supply
`/etc/bitblocker/config.yaml` yourself (the unit does not provision it), and
enable the unit:

```bash
sudo cp packaging/systemd/bitblocker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now bitblocker
```

`DynamicUser=true` creates a transient, unprivileged `bitblocker` user at
start — no manual `useradd` step is needed or possible. `CacheDirectory=bitblocker`
provisions `/var/cache/bitblocker` (mode `0750`, owned by that dynamic user)
before `ExecStart` runs, and — unlike a `RuntimeDirectory` — persists it
across restarts, which the disk-cache cold-start path depends on. The unit
also carries a full `systemd.exec(5)` hardening set (`ProtectSystem=strict`,
`NoNewPrivileges=true`, a narrowed `CapabilityBoundingSet=`, etc.) — read the
unit file's own comments for the specifics; an ansible role should install
it as-is rather than re-deriving the hardening directives.

## Configuration essentials

[`config.example.yaml`](../config.example.yaml) is the canonical template —
copy it to `/etc/bitblocker/config.yaml` (or wherever `--config` points) and
edit. The full field reference is the README's Configuration section; three
fields matter most at deploy time:

- **`block.countries`** — the ISO 3166-1 alpha-2 codes to block. At least
  one of `block.countries` or `block.asns` must be non-empty (ASN blocking
  is accepted by the schema but not implemented in v1).
- **`cache.path`** / **`cache.max_age`** — the on-disk blocklist snapshot
  (default `/var/cache/bitblocker/dbip-country-lite.mmdb`, default max age
  `48h`). This is the path that must sit on the persistent volume/directory
  described above.
- **`behavior.startup_mode`** — `fail-closed` (default) or `fail-open`.
  **Leave this at the default** unless you have a specific availability
  requirement — fail-closed is the safe and recommended setting for an
  edge-blocking daemon. If a deployment does set `fail-open`, read the
  README's ["Detecting a stuck fail-open state"](../README.md#detecting-a-stuck-fail-open-state)
  section before relying on it; it documents how to tell a brief cold-start
  blip from a daemon that has never worked.

## Client-IP trust — the most important thing to get right

The client IP BitBlocker blocks on is only as trustworthy as whatever hands
it that IP. This is a property of the *deployment's* Traefik configuration,
not of the daemon, and it is the single highest-consequence item in this
guide.

**BitBlocker trusts `X-Real-IP` (or, failing that, the rightmost entry of
`X-Forwarded-For`) as the client address** (see
[traefik-integration.md § Request contract](traefik-integration.md#request-contract-how-bitblocker-identifies-the-client)
for the exact extraction order and why rightmost-XFF is deliberate). Both of
those headers are only trustworthy if the component setting them is
rewriting from the real TCP peer it saw — not passing through whatever an
upstream client already sent. Two things must both be true for that to
hold:

1. **Traefik must be configured to overwrite `X-Real-IP` / `X-Forwarded-For`
   from the actual TCP connection**, not trust whatever a client sent in
   ahead of it. This is standard Traefik behavior when it terminates the
   connection directly; it stops being true the moment another proxy or CDN
   sits in front of Traefik and Traefik is told to trust *that* layer's
   headers uncritically. See
   [traefik-integration.md](traefik-integration.md) for the full contract
   and the `trustForwardHeader: true` setting this depends on.
2. **BitBlocker itself must not be reachable from anywhere an attacker could
   hit `/check` directly.** Bind `listen.host` to `127.0.0.1` (host/systemd
   mode) or an internal-only Docker network (container mode) — never publish
   BitBlocker's port on a public interface. If `/check` is reachable
   directly, an attacker bypasses Traefik entirely and can set `X-Real-IP`
   to anything, including an address that isn't blocked — a blocklist
   bypass with no trace of it going through the edge at all.

Get either of these wrong and the blocklist becomes decorative: a spoofed
`X-Real-IP` sails through as an allow. This is deployment-topology
configuration, not something BitBlocker's own config schema can enforce —
it is the one item an ansible role must get right independent of anything
in `config.yaml`.

## See also

- [traefik-integration.md](traefik-integration.md) — the full `forwardAuth`
  wiring walkthrough, header contract, and response-code table.
- [bitblocker-spec.md](bitblocker-spec.md) — design, HTTP API, data sources.
- [README](../README.md) — install paths, full config field reference, and
  fail-open troubleshooting.
- [ADR 0004](adr/0004-fail-open-wiring-and-readiness-observability.md) §E —
  the security-posture reasoning behind the fail-closed default.
