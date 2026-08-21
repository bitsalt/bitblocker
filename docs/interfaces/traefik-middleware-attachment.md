# Interface: Traefik middleware attachment on a shared multi-tenant host

> **Boundary owner:** the deployment's Traefik configuration (`bitsalt/ansible`,
> `roles/traefik` + the per-site roles). BitBlocker's own code is unchanged by
> anything in this document.
> **Consumers:** DevOps (the ansible role), the operator (the incident runbook),
> and any future role adding a site or a router to app01.
> **Governing standards:** `coding-standards.md` §14 (interface design), §4
> (explicit boundaries).
> **Decisions:** ADR 0005 (this contract), ADR 0004 (`/healthz` semantics — §9
> depends on them), ADR 0002 (the cache the volume in §2.3 must persist). In
> `bitsalt/ansible`: ADR 0020 (file-provider layout — §6.1 requires a stated
> deviation), ADR 0005 there (per-site named dict vars).

This spec is the contract DevOps builds the ansible role against. It states
**what must be true**, not how to render it — Jinja shape, task ordering, and
handler wiring are DevOps's. Where something is DevOps's call it is marked
**[DevOps's call]**.

**Status: this revision settles a conflict.** Two incompatible designs for this
contract were produced concurrently — `bitsalt/bitblocker#27` and
`bitsalt/ansible#253`. The contract below replaces both. If you are holding a
copy of either original, discard it. ADR 0005 § Reconciliation records what each
got right; ADR 0005 § What `bitsalt/ansible#253` must change is the delta list
for the ansible branch.

Do not re-derive the reasoning; it is in ADR 0005.

---

## 1. Purpose of the boundary

One BitBlocker daemon carries one country policy (`block.countries` is a single
flat global list, `internal/config/config.go:72-75`). app01 runs 18 site entries
behind one shared Traefik v3.6.13, and two of them must be exempt from country
filtering. `/check` decides from the client IP alone and never reads
`X-Forwarded-Host`, so **every per-site policy decision is made in Traefik, by
choosing which routers call `/check` at all.**

This boundary therefore has to carry five things the daemon cannot:

1. Which sites are declared for filtering (§4).
2. Whether filtering is actually happening right now (§5).
3. Which *routers* of a declared site are covered, and which must never be (§7).
4. How the state between "nothing deployed" and "fully deployed" is made
   harmless (§3, §8).
5. How the daemon's failure is prevented from becoming the host's failure (§8,
   §9, §11).

---

## 2. Daemon placement contract

### 2.1 Reachability — the hard requirement

**BitBlocker's `/check` must be reachable by Traefik and by nothing else.**

If `/check` is reachable from off-host, an attacker sets `X-Real-IP` to any
address and receives a 200, bypassing the blocklist entirely with no trace at
the edge (`docs/deployment.md` § Client-IP trust). This is not a hardening
nicety; it is the precondition that makes the control mean anything.

Satisfy it by attaching the container to the existing `proxy` Docker network
with **no published port** and `listen.host: 0.0.0.0` in its config, **or** by
binding `listen.host: 127.0.0.1` under systemd on the host. Do not publish
`8080` on any interface, including `0.0.0.0:8080` behind UFW. **[DevOps's call]**
which of the two run modes; both are supported by shipped artifacts
(`docs/deployment.md` § Run mode (a) / (b)).

### 2.2 Image pinning

Pin a specific tag — `ghcr.io/bitsalt/bitblocker:1.0.0` or `:1.0`. **Never
`:latest`** (`docs/deployment.md` § Image). A `:latest` deploy moves under a
running fleet on the next release.

### 2.3 Cache persistence — required, not optional

`cache.path` (default `/var/cache/bitblocker/dbip-country-lite.mmdb`) **must sit
on storage that survives container recreation** — a named volume or a host bind
mount. The `Dockerfile` declares a `VOLUME`, so an anonymous volume is created
even without `-v`, and an anonymous volume **does not survive `docker rm`**.

Losing the cache on every recreate means every apply that recreates the
container cold-starts the daemon fail-closed with no disk fallback. Under
container mode this is the single most consequential line in the compose file.
Under systemd mode, `CacheDirectory=bitblocker` in the shipped unit already
provides it — install the unit as-is rather than re-deriving its directives.

### 2.4 Config file

No config ships in the image; the daemon fails to start without one at
`--config` (default `/etc/bitblocker/config.yaml`). Values fixed by ADR 0005:

| Field | Value | Why |
|---|---|---|
| `behavior.startup_mode` | `fail-closed` | ADR 0005 §3.6. Do not set `fail-open`. |
| `behavior.log_blocked` | `true` (the default) | Required by the §10 V3 verification. |
| `cache.max_age` | `48h` (the default) | Do not raise it to work around the mtime defect (ADR 0005 § Alternatives). |
| `block.countries` | **operator input** | ADR 0005 OQ-A. Not DevOps's, not Architect's. |

### 2.5 Deployment gating is out of this contract

Whether the daemon's own deployment is gated by a role-inclusion variable, and
what that variable is called, is **[DevOps's call]** and does not enter this
spec. This spec constrains only what Traefik does. The single coupling is §5.2's
pre-flight check: enforcement is not rendered unless the daemon answers.

---

## 3. The contract in one table

Three independent controls, each with exactly one job.

| Name | Level | Type | Default | Gates | Cost to change |
|---|---|---|---|---|---|
| `geo_filter` | Site dict in `vars/sites/<site>.yml` | String enum `enforce` \| `exempt` | **None — required** | Whether that site's terminal routers emit `bitblocker@file` | Container recreate for that site; scope with `-e wordpress_site_filter=<site>` |
| `bitblocker_enforcing` | Fleet, `group_vars` | Boolean | **`false`** | Which body the `bitblocker` middleware is rendered with | One small file rewrite; **nothing restarted** |
| `bitblocker_address` | Fleet, `group_vars` | String, `host:port` | None | The `forwardAuth` target. Asserted present when enforcing | Same as above |

**The invariant that makes the whole thing safe — treat it as the first rule of
this document:**

> **The middleware named `bitblocker` is defined on every host where any router
> may reference it, in every rendering, unconditionally — including when
> `bitblocker_enforcing` is false and including when no BitBlocker daemon exists
> on that host at all.**

A request reaches the daemon only when **both** `geo_filter: enforce` and
`bitblocker_enforcing: true` hold. Until the second is deliberately flipped, the
entire apparatus stamps a request header and does nothing else.

**Why the split matters** (do not collapse these into one variable): "which
sites are filtered" changes rarely and costs a container recreate; "is filtering
happening" is an incident-time fact that must flip in seconds and reverse in
seconds. One variable for both puts every incident-time flip on the
12-container-recreate path. ADR 0005 §1.2.

---

## 4. The per-site declaration: `geo_filter`

Each site dict under `bitsalt/ansible` `vars/sites/<site>.yml` carries **one
required key**:

```yaml
<name>_site:
  enabled: true
  site_name: <name>
  geo_filter: enforce        # required — "enforce" | "exempt"
```

### 4.1 Contract

| Property | Value |
|---|---|
| Key name | `geo_filter` |
| Location | The site dict in `vars/sites/<site>.yml`, alongside `enabled` |
| Type | String, one of exactly `"enforce"` or `"exempt"` |
| Default | **None. There is no default.** |
| Omitted | **Fails the play**, with a message naming the site, both legal values, and the recommended value |
| Invalid value | **Fails the play**, same message |
| Assert scope | **Every enabled site dict in the play's vars, not only the ones a `*_site_filter` currently selects** |
| Assert timing | Before any template is rendered or any container recreated |
| Assert gating | **Unconditional** — not gated on `bitblocker_enforcing` |

The assert must be tagged so it runs on tag-scoped applies too (`tags: always`
on a `pre_tasks` assert, or equivalent). **[DevOps's call]** whether it is a
`pre_tasks` assert in `site.yml` or an assert at the top of each site role; the
requirements are only that it fires before the first change and that its scope
is every enabled site rather than the filtered subset.

**Why every enabled site, not the filtered subset.** With
`-e wordpress_site_filter=taotedev`, asserting only over the filter's selection
means a missing key on any other site stays hidden until the next full apply —
which is precisely the apply where you least want a new hard failure.

### 4.2 Why no default, and why a string

Stated so it is not "simplified" back into a defaulted boolean later.

**Why required rather than defaulted.** The strongest case for a default is the
fail-safe one — default `true` means an omitted flag blocks the site and fails
loudly rather than leaving it silently unprotected. That case is correct and it
defeats default-`false`. It does not defeat a required key, because the required
key has the same loudness *earlier and cheaper*: a default-`true` failure
surfaces at **traffic time**, detected by a visitor or a client hitting a 403 on
a live site; a required-key failure surfaces at **apply time**, before any
container is recreated, detected by the operator, costing nothing. Full
comparison, including the client-commitment angle, in ADR 0005 §1.3.

**Why a string enum rather than a boolean**, in order of weight:

1. **Omission is distinguishable from declaration.** With a defaulted boolean,
   "considered and chosen for enforcement" and "nobody thought about this site"
   are byte-identical config. Nothing restores that distinction after the fact.
2. **The exemption set is auditable.**
   `grep -c 'geo_filter: exempt' vars/sites/*.yml` is an exact count. A defaulted
   boolean's exemption set is its `false` entries *plus every site relying on the
   default*, which is not greppable — and that set is what OQ-B will need
   enumerated.
3. **Legibility, not coercion-safety.** YAML has several spellings of false
   (`no`, `off`, `n`) and `exempt` reads more reliably than `no` in review. Note
   that a typed `argument_specs` declaration already rejects an unparseable
   typo, so this is a readability argument rather than a safety one — do not
   over-claim it.

The assert message names `enforce` as the recommended value, because a required
key makes omission loud but does not by itself steer the next person toward the
safe answer. That steering is the one property a default has and this design
does not.

### 4.3 Applies to all three site families

`geo_filter` is required on WordPress, Node.js, and FastAPI site dicts alike.
Node.js and FastAPI sites are served by `roles/webapp`, which emits **no
`middlewares=` label today** — that plumbing does not exist and is Phase-5 work
(ADR 0005 §5.1). Until it exists those sites carry `geo_filter: exempt`, because
`enforce` on a site whose role cannot attach anything is a silent lie about
coverage.

---

## 5. The fleet enforcement switch: `bitblocker_enforcing`

### 5.1 Contract

| Property | Value |
|---|---|
| Name | `bitblocker_enforcing` |
| Location | `roles/traefik/defaults/main.yml` for the default; `group_vars` for the production override |
| Type | Boolean |
| **Role default** | **`false`** |
| Effect when `false` | The `bitblocker` middleware is rendered in its benign body (§6.3). Nothing calls the daemon. |
| Effect when `true` | The `bitblocker` middleware is rendered as `forwardAuth` (§6.2). |
| Effect on router labels | **None.** Labels are emitted from `geo_filter` alone. |

The default is `false` because this switch points a security control at a daemon
that may not exist. `false` is the correct behavior on every host that has not
been explicitly configured — including staging, including a rebuilt host,
including any future host.

**Renamed from `bitblocker_enabled`, and the default is inherited unchanged.**
`bitsalt/ansible#253` already ships `bitblocker_enabled: false` in
`group_vars/all/vars.yml`; that default is right and carries over as-is. The
rename is required because the variable's **meaning** changes: on that branch
`bitblocker_enabled` is one conjunct of the template's `item_bitblocker_enabled`
and therefore gates *label emission*; here the fleet switch selects the
middleware's *body* and never touches a label. Reusing the name would carry the
old semantics into the new template. Secondary: `..._enabled` invites the reading
"is BitBlocker installed on this host," which is a separate concern this spec
deliberately leaves to DevOps (§2.5).

### 5.2 Pre-flight asserts — required when enforcing

When `bitblocker_enforcing` is truthy, **before the enforcing body is rendered**:

1. `bitblocker_address` is defined and non-empty.
2. `GET http://{{ bitblocker_address }}/healthz` returns **200**.

A `/healthz` 200 means the daemon is up *and* holds a usable blocklist (ADR 0004
§C). A 503 means it is up but unusable — rendering the enforcing body against a
503 daemon fail-closes every attached site. Do not accept a 503 as "up."

**These asserts carry their own tag** so an operator can `--skip-tags` them for a
single apply. Precedent and shape: `roles/traefik/tasks/main.yml:98-107`, the
DNS-01 credentials task, which fails loudly by default and is skippable only as
an explicit one-apply opt-in. Without the skip tag, an unrelated apply during a
BitBlocker outage is blocked by this assert.

### 5.3 What this switch is and is not

It is a **fleet-wide enforcement toggle** and it is the lever the incident
runbook reaches for first (§11). It is not a per-site control (that is §4) and it
is not a way to remove BitBlocker (that is a full apply). It **never** changes
which routers carry the label.

---

## 6. The middleware definition

### 6.1 One definition, always present, in its own file-provider fragment

| Property | Value |
|---|---|
| Middleware name | `bitblocker` |
| Referenced from routers as | `bitblocker@file` |
| Rendered to | `{{ traefik_proxy_dir }}/dynamic/bitblocker.yml` |
| Rendered from | `roles/traefik/templates/bitblocker.yml.j2` **[DevOps's call on the template filename]** |
| Rendered when | **Always.** No `when:` guard on the render task. |
| Render task notifies `Restart traefik` | **No. Never.** |
| Defined via docker labels | **No.** |

**The task must not notify the restart handler.** The existing task that renders
`dynamic/middlewares.yml` does notify it (`roles/traefik/tasks/main.yml:48-55`),
and that handler recreates the Traefik container. If `bitblocker` lived in
`middlewares.yml`, every enforcement flip would recreate the shared edge proxy
for all 18 sites — destroying the one property that makes §11's lever 1 worth
having. `--providers.file.watch=true` is already set
(`roles/traefik/templates/docker-compose.yml.j2:40`), so a content change to
this fragment is picked up in seconds with no container touched.

**This is a deliberate deviation from `bitsalt/ansible` ADR 0020**, which
mandates splitting file-provider config by kind rather than by concern. The
deviation needs a short amendment filed in that ADR recording the reason (ADR
0005 OQ-J). Do not "tidy" this fragment back into `middlewares.yml`.

**It is not defined via docker labels on the daemon's container.** Doing so
would put the kill switch behind a container recreate and couple the
middleware's existence to the lifecycle of the very thing being toggled — so
stopping the daemon would delete the middleware and break every attached router.

### 6.2 Enforcing body — rendered when `bitblocker_enforcing` is true

```yaml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://{{ bitblocker_address }}/check"
        trustForwardHeader: true
```

`trustForwardHeader: true` is **required**, not cosmetic — it is what causes
Traefik to present the `X-Forwarded-For` / `X-Real-IP` it derived from the real
TCP peer (`docs/traefik-integration.md` § Register the middleware). Without it
the daemon has no client address to decide on and fails closed on every request.

### 6.3 Benign body — rendered when `bitblocker_enforcing` is false, and the default

```yaml
http:
  middlewares:
    bitblocker:
      headers:
        customRequestHeaders:
          X-BitBlocker-Enforcement: "disabled"
```

Requirements on this body:

- It is a **pass-through**, never an absent definition and never an empty
  `chain`.
- It **must** add the header. The header is not decoration: it is what makes
  §10's V0b able to *observe* that an attached-but-disabled middleware is inert,
  rather than assume it. Without a visible effect, "the benign middleware did
  nothing" and "the middleware was never applied" are indistinguishable.
- The header is inert to WordPress, Node, and FastAPI, and doubles as positive
  evidence in backend logs that enforcement is off.

### 6.4 The trap, stated as a hard constraint

> **The middleware named `bitblocker` must be defined in every rendering.** A
> Traefik router that references a middleware which does not exist is placed in
> an error state and stops serving its rule. "Disable enforcement by removing
> the middleware definition" would therefore take down every attached site —
> the exact opposite of a kill switch.

There is standing precedent for this hazard in the same directory:
`roles/traefik/templates/dynamic.yml.j2:56-60` warns that `asana-ip-allowlist`
is emitted only when non-empty and must therefore never be referenced from a
router label. `bitblocker` takes the opposite approach — always emitted — which
is what permits its label to be static.

Two corollaries:

- **Removing the fragment is not a rollback step.** It is an outage of every
  site whose labels still name it. Full removal (§11 lever 3) removes labels
  first and may leave the fragment in place indefinitely; the fragment is inert.
- **A YAML parse error in this fragment silently removes the middleware.** The
  file provider logs and skips an unparseable file rather than failing the
  container (ansible ADR 0020 § Consequences). Keep the template small,
  loop-free, and two-branch; rely on the repo's yamllint / ansible-lint pass.

---

## 7. Router attachment

### 7.1 The rule

> **For a site with `geo_filter: enforce`, every router the site role emits
> carries `bitblocker@file`, with exactly one exception: a router that already
> rejects every request unconditionally, where the middleware has no marginal
> effect. Routers serving machine traffic are never attached — that list (§7.4)
> is a property of the surface, not of any site's flag.**

Stated in terms of router *behavior* rather than router *names* so it survives
routers added after this document. The rule cannot under-attach, and it needs no
per-router judgment call — which is the point, since per-router drift is the
accepted cost of rejecting entrypoint attachment (§7.6).

**Aliases inherit the parent site's `geo_filter`.** There is no per-alias key.

**Labels are emitted from `geo_filter` alone.** They do not depend on
`bitblocker_enforcing`, on whether the daemon exists, or on any host fact other
than §7.5's guard. This is deliberate: it keeps the disruptive step (container
recreates) off the enforcement-flip path.

### 7.2 WordPress routers — the enumeration as of 2026-08-03

Verified against `roles/wordpress/templates/docker-compose.yml.j2`:

**Five routers carry it. One does not.**

| Router | Rule (line) | `middlewares=` label (line) | Carries `bitblocker@file`? |
|---|---|---|---|
| `{site}` (apex) | `Host(domain)` (118) | **121**, `{site}-block-xmlrpc` | **Yes** |
| `{site}-login` | `Host(domain) && Path(/wp-login.php)` (135) | **138**, `{site}-ratelimit-login` | **Yes — do not miss this one** |
| `{site}-www` | `Host(www.domain)` (182) | **185**, `{site}-www-redirect` | **Yes** |
| `{site}-alias-N` | `Host(alias.from_domain)` (198) | **201**, `{site}-alias-N-redirect` | **Yes** |
| `{site}-alias-N-www` | `Host(www.alias.from_domain)` (226) | **229**, `{site}-alias-N-www-redirect` | **Yes** |
| `{site}-block-batch` | `Host(domain) && POST && batch paths` (154) | 157 | **No — the sole exception.** It already 403s every request via a TEST-NET-1 `ipAllowList` sourcerange, so the middleware has zero marginal effect and would only add the daemon to this router's dependency set. |

**`{site}-login` is the attachment that must not be missed.** Its rule is longer
than the apex router's, so it wins match priority; attaching only to the apex
leaves the WordPress login form — the single highest-value target on these sites
— reachable from every blocked country. This is not a hypothetical failure mode
in this repo: the comments at lines 122-134 and 140-153 document two controls
that were inert in production for months because a per-router attribute was
missing on exactly these two routers.

**Why the three redirect-only routers are attached even though they cannot leak
content.** A blocked visitor to `www.<site>` or an alias would otherwise get a
301 and be blocked one hop later at the apex — the same security outcome, one
round trip later. They are attached anyway because the resulting rule is simpler
and cannot under-attach, and because serving a 301 to a visitor already decided
against is work done on their behalf. Reasoning in full: ADR 0005 §1.6.1.

**Attaching to these routers does not endanger certificates.** `-www`,
`-alias-N` and `-alias-N-www` all carry `tls.certresolver=le` — *standalone
DNS-01* orders (see the comments at 160-181 and 207-223) — so no inbound HTTP
validation traverses them. The HTTP-01 concern is real but lives on the `web`
entrypoint and is covered by §7.4.

### 7.3 Chain position

`bitblocker@file` goes **first** in every router's comma-separated middleware
list, without exception:

```
traefik.http.routers.{{ item.site_name }}.middlewares=bitblocker@file,{{ item.site_name }}-block-xmlrpc
traefik.http.routers.{{ item.site_name }}-login.middlewares=bitblocker@file,{{ item.site_name }}-ratelimit-login
traefik.http.routers.{{ item.site_name }}-www.middlewares=bitblocker@file,{{ item.site_name }}-www-redirect
traefik.http.routers.{{ item.site_name }}-alias-{{ loop.index }}.middlewares=bitblocker@file,{{ item.site_name }}-alias-{{ loop.index }}-redirect
traefik.http.routers.{{ item.site_name }}-alias-{{ loop.index }}-www.middlewares=bitblocker@file,{{ item.site_name }}-alias-{{ loop.index }}-www-redirect
```

Traefik applies the chain in order. Placing the geo decision first means a
blocked request is rejected before any rewrite or rate-limit work is done, and
means the decision is never taken against a path some earlier middleware
rewrote. **On the three redirect routers this is the whole point of attaching
at all** — first position is what makes the visitor receive a 403 instead of a
301; any later position would be indistinguishable from not attaching.

### 7.4 Routers that must NEVER carry it

| Surface | Why |
|---|---|
| The `web` entrypoint / ACME HTTP-01 challenge path | The `le-http` resolver (`roles/traefik/templates/docker-compose.yml.j2:65-68`) validates over port 80 from Let's Encrypt's vantage points, which are multi-regional. Filtering that path breaks certificate renewal — silently, ~60 days before anyone notices. DNS-01 (`le`) is unaffected. |
| `portfolio-service` `/internal/*` (GitHub + Asana webhooks) | Already constrained by `internal-ip-allowlist`. A second, geo-shaped failure mode on a push-triggered sync buys nothing. |
| Any external uptime monitor or synthetic-check path | Must be enumerated before enforcement is turned on (ADR 0005 OQ-F). There is no per-IP allowlist in v1 (OQ-3, still open), so a blocked monitor cannot be excepted except by unattaching the whole site. |

### 7.5 The fragment-presence guard — required

> **A site role must not emit `bitblocker@file` unless
> `{{ traefik_proxy_dir }}/dynamic/bitblocker.yml` is present on the host. If it
> is absent and the site is `geo_filter: enforce`, the play fails**, naming the
> site and instructing the operator to include the `traefik` tag in the apply.

**Why this exists.** In a full `site.yml` apply the ordering is automatic:
`traefik` is in the `roles:` block (`site.yml:185`) and every site family is
deployed from the `tasks:` block (`site.yml:221-256`), which runs after all
roles. The reachable violation is a **tag-scoped apply** — `--tags wordpress`
against a host whose `traefik` role has not been re-applied since this change
would emit labels naming a middleware that is not there, and §6.4 says what
happens next.

**Fail, do not silently omit.** Omitting would leave a site the operator
believes is protected silently unprotected — the exact decorative-control
failure this repo has documented history of. Failing costs nothing: the check
runs before any template is rendered, so nothing has changed when it fires, and
the remedy is one more tag on the command line.

Give it its own tag so it can be deliberately skipped for one apply, same shape
as §5.2. **[DevOps's call]** whether the check is a `stat` in each site role or a
once-per-play fact gathered in `pre_tasks`.

This converts an ordering requirement from a procedure into a property.

### 7.6 Never at the entrypoint

**Do not add `bitblocker@file` to
`--entrypoints.websecure.http.middlewares`** (`roles/traefik/templates/docker-compose.yml.j2:50`).
Traefik's `forwardAuth` to an unreachable address returns **500 for the original
request**, so any BitBlocker process failure — stopped, crashed, OOM-killed,
mid-pull, wedged — would take down every router on the entrypoint, including
both exempt clients, `portfolio-service`, and umami. Per-router attachment is
what makes the exemption structural rather than dependent on the exempting
process being alive. It would also put the filter in front of every surface in
§7.4. Full reasoning: ADR 0005 §1.7.

---

## 8. State semantics — what each state does to each site

### 8.1 Deployment states

Read this table before proposing any change to §3's invariant or §5.1's default.
It is the answer to "what happens if the template has landed but the daemon does
not exist yet."

| State | What exists | `geo_filter: enforce` site | `geo_filter: exempt` site |
|---|---|---|---|
| Fragment rendered, benign body, no labels, no daemon | A defined, unreferenced middleware | **Normal** | **Normal** |
| Labels emitted, benign body, **no daemon** | Routers reference a middleware that stamps a header | **Normal**, plus `X-BitBlocker-Enforcement: disabled` at the backend | **Normal** — no label, no header |
| Daemon deployed, benign body | Daemon running, nothing points at it | **Normal** | **Normal** |
| `bitblocker_enforcing: true`, daemon healthy | Live | Filtered | **Normal** |

The second row is the state the operator's constraint is about, and it is a
no-op. It is verified, not assumed — §10 V0b.

### 8.2 Failure states, once enforcing

| BitBlocker state | `geo_filter: enforce` site | `geo_filter: exempt` site | Non-attached router (ACME, `/internal/`) |
|---|---|---|---|
| Healthy, blocklist usable | Enforcing: block status for blocked countries, 200 otherwise | **Unaffected** | **Unaffected** |
| Running, blocklist UNUSABLE (`fail-closed`) | **403 for everyone** | **Unaffected** | **Unaffected** |
| Process down / unreachable | **Traefik 500 for everyone** | **Unaffected** | **Unaffected** |
| `bitblocker_enforcing: false` | 200, unfiltered, header stamped | Unaffected | Unaffected |
| `bitblocker` middleware undefined (§6.4 violation) | **Router in error state — site down** | **Unaffected** | **Unaffected** |

The two right-hand columns are the point of this design. Read them before
proposing entrypoint attachment.

---

## 9. Health checking — a hard constraint

> **BitBlocker's container declares no `HEALTHCHECK`.**

`roles/wp_container_autoheal` installs a host-side timer running
`docker ps --filter health=unhealthy` and restarts **any** container on app01
reporting unhealthy (`templates/autoheal.sh.j2:41-53`). It is fleet-scoped, not
WordPress-scoped (`site.yml:174-183`).

A `HEALTHCHECK` wired to `/healthz` composes with that into a harmful loop:
unusable blocklist ⇒ `/healthz` 503 (deliberately — ADR 0004 §C) ⇒ container
unhealthy ⇒ autoheal restarts ⇒ the restart re-runs `loadDiskCache`, which
**deletes** a stale cache and resets the cold-start retry budget ⇒ still
unusable ⇒ restart again at the cooldown interval. Every iteration destroys
recovery state and gains nothing: **restarting a daemon that has no blocklist
does not give it a blocklist.**

`/healthz` is a **readiness** signal, not a liveness signal. Monitor it
externally instead:

- Poll `/healthz` from the host (`200` + `"serving":"enforcing"` + `prefixes > 0`
  is the healthy shape) and alert; **do not restart on it.**
- Alert on the daemon's mandatory ERROR heartbeat via the existing Vector→Loki
  path: log message `check: blocklist still unusable`, emitted every 60s while
  the state persists, not suppressible by any config (ADR 0004 §D.2).

The shipped image is distroless with no shell or fetch tool, so the naive
`HEALTHCHECK` does not work anyway. This constraint exists so a well-intentioned
wrapper image does not reintroduce it.

**[DevOps's call]:** whether the external `/healthz` poll is a systemd timer, a
Vector check, or a Loki-side alert on the heartbeat alone.

---

## 10. Verification obligations

Detail and exact commands: ADR 0005 §5.2. Summarized here because these are gate
conditions on the rollout, not optional extras.

| # | When | Check | Gates |
|---|---|---|---|
| **V0** | Fragment rendered, no labels | Traefik logs no file-provider parse error for `dynamic/bitblocker.yml`; the middleware appears in Traefik's runtime config; **all 18 sites still serve**. | Attaching any label |
| **V0b** | Pilot site attached, enforcement off | Pilot serves 200 through Traefik on :443 **and** the backend receives `X-BitBlocker-Enforcement: disabled`. | Attaching the rest of the fleet |
| **V1** | Daemon deployed, still not enforcing | `/check` returns the block status for a blocked-country IP and 200 for a US IP, via direct `curl` on the host. `/healthz` is 200 with `prefixes > 0`. Restart the daemon and confirm `/healthz` returns to 200 with no visible window. | Turning enforcement on |
| **V2** | Enforcing, pilot only | End-to-end through Traefik on :443 from a real egress, using a temporary **canary country** added to `block.countries`, then removed. Proves the whole chain, not just the daemon. | Leaving enforcement on |
| **V3** | Enforcing, pilot only | **The inert-control check.** Confirm the redacted client addresses in the daemon's logs **vary across requests** rather than being one constant `172.21.x.x`. | Leaving enforcement on |
| **V4** | Enforcing | Flip `bitblocker_enforcing` false and back. Confirm Traefik picks up each change within seconds, the pilot keeps serving, and **`docker ps` shows the Traefik container's uptime unchanged across both flips.** | Trusting §11 lever 1 |

**V0b is the check that discharges the operator's acceptance condition.** It is
the empirical form of §8.1's second row: attached, no daemon, site serving
normally, header proving the middleware really is in the chain. Skipping it
means the harmlessness of the intermediate state is an argument rather than an
observation.

**V3 is the one most likely to be skipped and the one with precedent.**
BitBlocker takes the **rightmost** `X-Forwarded-For` entry. If Traefik's
`forwardAuth` call presents XFF as `<client>, <traefik-internal-IP>` rather than
a bare `<client>`, the rightmost entry is Traefik's own address — in no blocked
country — and the daemon allows everything while looking perfectly healthy:
`/healthz` 200, populated trie, no errors, no signal anywhere. The control would
be entirely decorative and V1 would not reveal it.

The header contract is *expected* to be clean on app01: Traefik scopes
`forwardedHeaders.trustedIPs` to the docker-proxy subnet
(`roles/traefik/templates/docker-compose.yml.j2:44,46`), so an untrusted public
peer's own XFF is discarded and rewritten from the real TCP peer, and
`ansible/lessons/umami-geo-granularity-client-ip-header.md` confirms nothing
fronts app01. **That is a reason to expect V3 to pass, not a reason to skip it.**

**V4 tests the property, not the outcome.** "Enforcement turned off" is easy to
confirm; "and nothing restarted" is the claim that the design rests on and the
one that a fold-back into `middlewares.yml` would silently break.

---

## 11. Rollback levers — ordered, and the anti-levers

1. **Fleet-wide, ~10 seconds, no restarts:** `bitblocker_enforcing: false`,
   render `dynamic/bitblocker.yml` in its benign body (§6.3). No container
   restarted, no router label changed, no site role re-applied. **This is the
   incident lever.** Its restart-free property depends entirely on §6.1's
   no-notify requirement.
2. **Per-site, minutes:** `geo_filter: exempt` on one site, re-apply that site's
   role with `-e wordpress_site_filter=<site>` (`site.yml:23-27`). Costs a
   container recreate on that site.
3. **Full removal, one apply:** all sites to `exempt` (labels drop), then remove
   the daemon. **Leave the fragment in place** — it is inert, and removing it
   while any label survives anywhere is an outage.

**Anti-lever 1 — put this in the runbook in these words:**

> **Do not stop, kill, or `docker rm` the BitBlocker container to "turn it
> off."** A `forwardAuth` middleware pointing at a dead address makes Traefik
> return **500 for every attached site**. Stopping the daemon converts a partial
> problem into a total outage of everything it is attached to. Use lever 1.

**Anti-lever 2:**

> **Do not "disable" by deleting the middleware definition or the fragment
> file.** §3's invariant is what makes every label safe. Lever 1 changes the
> fragment's *body*, never its existence.

Lever 1 exists specifically so neither anti-lever is the reachable option under
incident pressure, where the instinct — stop the thing causing trouble — is
exactly wrong.

---

## 12. Versioning stance

- **`/check`'s status-code semantics are the `forwardAuth` contract** and are
  unchanged by anything here. Nothing in this document alters BitBlocker's code.
- **`geo_filter` is a new required key on an internal ansible data structure.**
  Adding it is a breaking change to every site vars file: all 18 must carry it
  before the next apply. That is deliberate (§4.2) and means the ansible change
  cannot land piecemeal.
- **`bitblocker@file` is a name other roles reference.** Renaming it later breaks
  every router label that names it and, per §6.4, takes those routers out of
  service rather than merely unfiltering them. Treat the name as a published
  contract. Renaming requires: rename in the fragment, apply `--tags traefik`,
  then re-apply every site — in that order, never the reverse.
- **`dynamic/bitblocker.yml`'s path is part of the contract**, because §7.5's
  guard stats it. Moving the file requires updating the guard in the same change.
- **The fragment's no-notify property is part of the contract**, not an
  implementation detail. A future tidy-up that folds it into `middlewares.yml`
  or adds a notify silently converts the incident lever into a fleet-wide proxy
  recreate. V4 is the regression test.
- **§7.2's router enumeration is a point-in-time snapshot.** §7.1's rule is the
  durable contract; the table is the current application of it. A router added
  to a site role after this date must be evaluated against §7.1 — that
  evaluation is the mitigation for the drift risk ADR 0005 § Consequences
  accepts.

---

## 13. Cross-references

- `docs/adr/0005-shared-host-per-site-policy-attachment-and-fail-mode.md` — the
  decision and its reasoning, including § Reconciliation (why this contract
  replaced two earlier ones) and § What `bitsalt/ansible#253` must change.
- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` §C, §D —
  `/healthz` readiness semantics and the mandatory ERROR heartbeat that §9
  depends on.
- `docs/interfaces/cache-freshness-and-stale-fallback.md` — the Developer-side
  changes (Fix A / Fix B) that bound the fail-closed window; Fix A should land
  before enforcement is turned on.
- `docs/deployment.md` § Image, § Run mode (a)/(b), § Configuration essentials,
  § Client-IP trust.
- `docs/traefik-integration.md` § Register the middleware, § Request contract,
  § Response contract, § `/healthz` as a health check.
- `bitsalt/ansible`: `site.yml:23-27, 174-183, 185, 221-256`;
  `roles/traefik/tasks/main.yml:40-55, 98-107`;
  `roles/traefik/templates/docker-compose.yml.j2:39-40,44,46,50,65-68`;
  `roles/traefik/templates/dynamic.yml.j2:56-60`;
  `roles/wordpress/templates/docker-compose.yml.j2` — apex `:118-121`, `-login`
  `:122-138`, `-block-batch` `:140-158`, `-www` `:159-189` (standalone DNS-01
  rationale at `:160-181`), `-alias-N` `:190-205`, `-alias-N-www` `:206-234`
  (standalone DNS-01 rationale at `:207-223`);
  `roles/wordpress/meta/argument_specs.yml` — `item.options`, where
  `bitblocker_country_block_enabled` is declared on `bitsalt/ansible#253`;
  `group_vars/all/vars.yml` — `bitblocker_enabled: false` as shipped on that
  branch, the default §5.1 inherits;
  `roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`;
  ADR 0020 (file-provider layout — see §6.1's deviation), ADR 0005 there
  (per-site named dict vars).
- Coding standards §14, §4.
