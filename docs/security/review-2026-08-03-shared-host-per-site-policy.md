# Security review — shared-host per-site policy attachment and fail-mode

- **Date:** 2026-08-03
- **Reviewer:** Security Engineer
- **Operation:** REVIEW (design-time)
- **Subject:** ADR 0005 (`docs/adr/0005-shared-host-per-site-policy-attachment-and-fail-mode.md`),
  `docs/interfaces/traefik-middleware-attachment.md`, and the counterpart
  deployment design `bitsalt/ansible#253`
  (`docs/ops/bitblocker-deployment-plan.md`), read at
  `bitsalt/bitblocker#27` HEAD `e7d4328`.
- **Context read:** ADR 0004 + `docs/interfaces/fail-open-and-readiness.md`,
  `docs/deployment.md` § Client-IP trust, `docs/traefik-integration.md`,
  `internal/server/clientip.go`, `internal/server/server.go`, and the live
  `bitsalt/ansible` tree at `main` (`roles/traefik/templates/docker-compose.yml.j2`,
  `roles/wordpress/templates/docker-compose.yml.j2`, `site.yml`,
  `roles/traefik/defaults/main.yml`).
- **Standing operator constraint honored throughout:** *the Traefik routers going
  down on the app01 fleet is the outcome to avoid.* Every recommendation below
  states its router risk. Two of them (S-6, S-10) touch the shared edge proxy and
  are flagged inline.

## Why this review exists

Security was not run on this design during the planning pass. Architect performed
security analysis inline — the `X-Forwarded-Host` bypass path and the
rightmost-XFF trust contract are discussed in ADR 0005 §4.2 and §5.2. This review
does not audit that analysis; the trust surface was re-derived from the daemon's
code and the live ansible tree, and the findings below are what that derivation
produced. Where a finding agrees with the ADR, it says so.

## Verdict

**Two blocking findings and one blocking-for-handoff finding. The design is
sound; the blocking items are gaps in what it *requires* and *verifies*, not
errors in what it decides.**

Nothing here argues against the three-control shape, the always-defined-middleware
invariant, the static-attachment/dynamic-enforcement split, the default-off fleet
switch, or the fail-closed posture. Those are right, and §2's argument that the
intermediate state is harmless holds up against the live templates. The gaps are:

1. The control's integrity rests on an ansible entrypoint setting that neither
   document states as a requirement (**S-1**).
2. The verification suite cannot observe the failure mode most likely to make the
   control decorative (**S-2**).
3. Two hard requirements in the interface spec contradict each other, and the
   ansible plan has already resolved the contradiction the other way (**S-3**).

All three are cheap to close and none of their mitigations touch a router.

---

## Findings

Severity: `critical` | `high` | `medium` | `low` | `info`.
Blocking status is against the handoff to Developer/DevOps for implementation.

---

### S-1 — `X-Real-IP` is an attacker-controlled input path on app01, and nothing in the settled contract requires the control that closes it

**Severity: high. Blocking.**

**Where.** `internal/server/clientip.go:31-45`; ADR 0005 §5.2 (V3 and its "the
header contract is believed clean on app01" paragraph); interface spec §6.2 and
§10; `bitsalt/ansible` `roles/traefik/templates/docker-compose.yml.j2:44,46`;
`bitsalt/ansible#253` §5.

**What the code actually does.** `extractClientIP` takes `X-Real-IP` **first**,
unconditionally, and only falls through to rightmost-XFF when `X-Real-IP` is
absent or unparseable:

```go
if v := strings.TrimSpace(r.Header.Get(headerXRealIP)); v != "" {
    if addr, err := netip.ParseAddr(v); err == nil {
        return addr.Unmap(), true
    }
}
```

So the decision is made on `X-Real-IP` whenever one arrives. Traefik's
`forwardAuth` copies the original request's headers wholesale to the auth
target, and `trustForwardHeader` governs only the `X-Forwarded-Method` /
`-Proto` / `-Port` / `-Host` / `-Uri` family — `X-Real-IP` is not in that family
and is not a hop-by-hop header, so it is not filtered on the forwardAuth leg.

**What closes it, and why that is the problem.** The only thing preventing a
client-supplied `X-Real-IP` from reaching the daemon is that Traefik's
entrypoint-level forwarded-headers handling **strips** `X-Real-Ip` from requests
whose TCP peer is not in `forwardedHeaders.trustedIPs`. On app01 that is
`--entrypoints.{web,websecure}.forwardedHeaders.trustedIPs={{ traefik_docker_proxy_subnet }}`
— a private range, so every public client is untrusted and the header is
stripped. The protection is real and it is currently in place.

It is also **entirely outside the contract this design writes down.** ADR 0005
§5.2 mentions `trustedIPs` once, as *evidence that V3 is expected to pass*.
Interface spec §6.2 does not mention it. The ansible plan (§5) states it as a
fact about the current topology — "already verified true, not a new check" —
rather than as an invariant to preserve. Nothing anywhere says: *widening
`forwardedHeaders.trustedIPs` to include a public range, or setting
`forwardedHeaders.insecure` on either entrypoint, is a total, silent bypass of
BitBlocker.*

**Why that matters more than it looks.** The decay path is concrete and is one
the ADR itself anticipates in another form. ADR 0005 §5.2 names "a CDN placed in
front of one site later" as a residual risk to the *XFF* contract. If that
happens, the natural remediation — the one every Traefik-behind-CDN guide
prescribes — is to add the CDN's ranges to `forwardedHeaders.trustedIPs`. At
that moment every request arriving via the CDN has its client-supplied
`X-Real-IP` preserved and forwarded to the daemon, and any client from a blocked
country sets `X-Real-IP: 8.8.8.8` and is allowed. The operator making that change
would be fixing a real problem, following standard advice, with no indication
anywhere in the ansible repo or in BitBlocker's docs that they had just disabled
the geo filter fleet-wide.

The asymmetry that makes this worse than the XFF case the docs *do* treat
carefully: rightmost-XFF is defensible under an untrusted-peer topology because
Traefik appends the real peer on the right. `X-Real-IP` has no such structure —
it is a single value, it takes priority, and it is either trustworthy or a total
bypass with nothing in between.

**One further observation, worth confirming rather than assuming.** In this
topology Traefik does not *generate* `X-Real-IP` — it only deletes an untrusted
one. If that is so, then on app01 the first branch of `extractClientIP` is never
taken by a legitimate request, and is exclusively an attacker-reachable input
path with zero operational benefit. `docs/deployment.md:132-134` instructs the
operator to "configure Traefik to overwrite `X-Real-IP` … from the actual TCP
connection," which is not a thing Traefik has a setting to do; the real mechanism
is deletion, and the doc's phrasing invites an operator to go looking for a knob
that does not exist and conclude the requirement is already met by something
else.

**Recommended mitigation** (three parts, none of which touches a router):

1. **State it as a contract requirement.** Add to interface spec §2 (daemon
   placement) a subsection with the same weight as §2.1's reachability rule:
   *every entrypoint carrying a `bitblocker@file`-attached router must keep
   `forwardedHeaders.trustedIPs` scoped to non-public ranges and must never set
   `forwardedHeaders.insecure`; changing either requires re-deriving the
   client-IP contract before the change ships.* Mirror it as a comment at the
   `forwardedHeaders.trustedIPs` lines in
   `roles/traefik/templates/docker-compose.yml.j2` — the same idiom the repo
   already uses for the `asana-ip-allowlist` warning at
   `dynamic.yml.j2:56-60`, which is the in-repo precedent for exactly this kind
   of standing hazard note.
2. **Verify it empirically** — see S-2, which specifies the check.
3. **Hand to Developer as a v1.1 item (not blocking this rollout):** confirm
   whether Traefik ever sets `X-Real-IP` in this topology; if it does not, either
   make the trusted-header list configurable or drop `X-Real-IP` priority behind
   an explicit opt-in. A header the deployment never legitimately produces should
   not outrank the one it does. Correct `docs/deployment.md:132-134` to describe
   deletion rather than overwriting in the same pass.

**Router risk of the mitigation: none.** Items 1 and 3 are documentation and a
future code change; item 2 is two `curl` invocations.

---

### S-2 — the verification suite cannot detect a header-spoofing bypass; V3 detects only the shape that is already expected to pass

**Severity: high. Blocking.**

**Where.** ADR 0005 §5.2 (V0–V4); interface spec §10.

**Why.** V3 is the design's inert-control check, and its detection signature is
"the redacted client addresses in the daemon's logs are one constant
`172.21.x.x`" — the signature of Traefik presenting its own address as the
rightmost XFF entry. That is a real failure mode and V3 is right to check it. But
it is the failure mode the ADR itself argues is *unlikely* on this host.

The failure mode from S-1 produces the opposite signature. A client supplying
`X-Real-IP` produces **varied, plausible, non-blocked** addresses in exactly the
distribution a healthy daemon produces. V3 passes. `/healthz` returns 200 with
`prefixes > 0`. The heartbeat is silent. V1 passes, because V1 tests the daemon
in isolation with headers the operator sets by hand. V2 passes, because V2 tests
from an egress that does not spoof. Every check in the suite is green and the
control is off.

So the suite verifies that the daemon works and that Traefik reaches it, and does
not verify the one property the whole control rests on: **that a client cannot
influence the address the decision is made on.**

**Recommended mitigation — add V5, two `curl` invocations at Phase 4, no infra
change:**

> **V5 — the header-sanitization check (Phase 4, pilot only, run with the V2
> canary country still in `block.countries`).**
>
> From a **US egress**, against the pilot site through Traefik on :443:
> ```
> curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Real-IP: <IP in the canary country>' https://<pilot>/
> # MUST be 200. A block status means Traefik forwarded the client-supplied
> # X-Real-IP and the daemon decided on it — S-1's bypass, in the reverse
> # direction. Stop the rollout.
>
> curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: <IP in the canary country>' https://<pilot>/
> # MUST be 200. A block status means a client-supplied XFF entry reached the
> # rightmost position.
> ```
>
> From the **canary-country egress** used for V2:
> ```
> curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Real-IP: 8.8.8.8' https://<pilot>/
> curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: 8.8.8.8' https://<pilot>/
> # BOTH must be the block status. A 200 on either is a live bypass.
> ```
>
> **Gates: leaving enforcement on.** V5 is the check that makes S-1's protection
> an observation rather than an inference, and it is the regression test for any
> future change to `forwardedHeaders.trustedIPs` or to the fronting topology.

The US-egress half is worth as much as the blocked-egress half: it is the half
that still works after the canary country is removed, and it can be re-run at any
time against any enforcing site without needing a foreign egress.

**Router risk: none.** Four `curl` invocations against one pilot site.

---

### S-3 — the pre-flight `/healthz` assert and the external readiness poll are unimplementable under §2.1's no-published-port rule, and the ansible plan has already resolved the contradiction the other way

**Severity: medium. Blocking for the DevOps handoff.**

**Where.** Interface spec §2.1 vs. §5.2 and §9; ADR 0005 §1.4; `bitsalt/ansible#253`
§1 and §5.

**The contradiction.** Interface spec §2.1 states as a hard requirement: attach
the container to the `proxy` network **with no published port**, and "Do not
publish `8080` on any interface, including `0.0.0.0:8080` behind UFW." With no
published port, the daemon is reachable only from inside the `proxy` network.

But §5.2 requires, as a gate on rendering the enforcing body,
`GET http://{{ bitblocker_address }}/healthz` returning 200 — an ansible `uri`
task that runs from the controller or on the host, neither of which is on the
`proxy` network. And §9 requires "Poll `/healthz` from the host … and alert."
Both are outside the network the daemon is on. As written, the two requirements
cannot both be satisfied.

`bitsalt/ansible#253` has already resolved it, in the direction §2.1 forbids: §1
gives the operational check as
`ssh app01 "curl -s http://127.0.0.1:8080/healthz"`, and §5's networking table
specifies **`127.0.0.1:8080:8080` (loopback only, never `0.0.0.0:8080:8080`)**,
citing BitBlocker's own deployment guide as its reason. The reconciliation in ADR
0005 § What `bitsalt/ansible#253` must change does not mention this; item 8
("no published port") restates §2.1 without noticing that the ansible plan
already disagrees and that §5.2 needs the disagreement resolved.

**Why it matters.** The likely outcome of shipping the contradiction is that
DevOps hits it during implementation and resolves it under time pressure. The
resolution reached in a hurry may be `-p 8080:8080`, which is the genuinely
dangerous form: Docker's published-port DNAT rules are installed ahead of UFW's
INPUT chain, so a `0.0.0.0` publish is reachable from the internet regardless of
`ufw_default_incoming: deny` — and a directly reachable `/check` is the total
bypass `docs/deployment.md:140-146` describes. The spec knows this hazard; it
just does not say which of its own two requirements yields.

**Recommended mitigation.** Decide it in the spec rather than leaving it to
implementation. The defensible resolution is the one the ansible plan already
reached — **permit `127.0.0.1:8080:8080` explicitly, forbid every other publish
form, and say why**: a loopback publish is not reachable off-host and does not
interact with the Docker/UFW ordering hazard, while a `0.0.0.0` publish is
reachable from the internet even behind UFW. Amend interface spec §2.1 to state
the permitted form, and amend §5.2 and §9 to name `127.0.0.1:8080` as the address
the assert and the poll use. If the alternative is preferred (no publish at all,
checks run via `docker exec` or a `docker run --rm --network proxy` probe), state
*that*, and note that §5.2's assert then requires a container invocation rather
than the `uri` module.

Either way the reachability property that matters is preserved. What must not
happen is that this gets resolved on the host, once, by someone reading §5.2 and
adding a port mapping.

**Router risk: none.**

---

### S-4 — the risk arithmetic is computed on a filtered set roughly 60% larger than the contract's own rules produce

**Severity: medium. Non-blocking.**

**Where.** ADR 0005 §3.1 ("One daemon serving ~16 filtered sites"), §3.4 and §3.6
("16 sites down", "16 filtered tenants", "16 sites unfiltered"); against §5.1
Phase 5 and interface spec §4.3.

**The arithmetic.** The fleet is 18 site entries: 12 WordPress, 5 Node.js, 1
FastAPI (`site.yml:89-141`, verified). Interface spec §4.3 requires the 5 Node.js
and the 1 FastAPI site to carry `geo_filter: exempt` until Phase-5 plumbing
exists, because `roles/webapp` emits no `middlewares=` label. ADR 0005 OQ-H leans
toward excluding umami. Two of the WordPress sites are the exempt hosting
clients. So at Phase 4 the filtered set is **10 of 18 site entries**, not 16.

**Which way the error cuts.** It overstates blast radius, which is the input to
the fail-open trade-off — so correcting it *strengthens* ADR 0005 §3.6's
conclusion rather than weakening it (see the posture verdict below). It also
overstates coverage, which is the input to how the control is described to Jeff
and to clients: at Phase 4 this is a control on 10 client WordPress sites, with 8
site entries on the same host unfiltered by design.

**Recommended mitigation.** Correct the figure in §3.1, §3.4 and §3.6, and state
the Phase-4 filtered set explicitly (`12 WordPress − 2 exempt = 10`) alongside
the 18-entry fleet. It changes no decision and it stops a wrong number from being
cited into the next document.

**Router risk: none.**

---

### S-5 — "structurally immune" overclaims: a slow daemon degrades all 18 sites, and `forwardAuth` has no timeout to bound it

**Severity: medium. Non-blocking, but it should be closed before Phase 4.**

**Where.** ADR 0005 §1.7 and § Consequences ("unaffected by any BitBlocker state
— unusable blocklist, wedged process, stopped container, bad country list, or a
bug in this design"); interface spec §8.2's failure table.

**The gap.** §8.2 enumerates three daemon states: healthy, unusable-blocklist,
and "process down / unreachable." It does not enumerate the state between the
last two — **process alive, accepting connections, not answering.** That state
behaves differently from all three, and it is the one that reaches the exempt
sites.

A dead daemon refuses the TCP connection and Traefik returns 500 immediately: a
fast, bounded, loud failure confined to attached routers, exactly as §8.2
describes. A *hung* daemon accepts the connection and never responds. Traefik's
`forwardAuth` middleware exposes no timeout setting, so the auth call — and with
it the original request — waits on the transport default. Requests to the 10
attached sites accumulate, holding Traefik connections and goroutines, which is
**shared capacity across all 18 routers**. Exempt sites are immune to
BitBlocker's *decisions*; they are not immune to its *resource consumption of
the shared edge proxy*. §1.7's word "wedged" appears in the list of things
exempt clients are explicitly said to be immune to, and that is the one entry in
the list that does not hold.

The daemon's own timeouts do not help: `internal/server/server.go:137` sets
`ReadHeaderTimeout: 5 * time.Second` and no `WriteTimeout`, and neither bounds a
process that has stopped scheduling work.

This also interacts with §5.4/§9, which correctly forbid a `HEALTHCHECK` wired to
`/healthz` — `/healthz` is readiness and restarting on it destroys recovery
state. That reasoning is right. But §9 replaces the healthcheck with an external
**readiness** poll and puts nothing in place for **liveness**. `restart:
unless-stopped` recovers a crash-exit; it does not recover a hang. So the
deployment has no detection and no remediation for the one failure mode that
reaches every tenant.

**Recommended mitigation** (in order of cost):

1. **Set a memory limit on the daemon's container** (`mem_limit` / `deploy.resources.limits.memory`).
   This converts the most likely hang cause — memory pressure and GC thrash — into
   an OOM kill, which is a crash-exit, which `restart: unless-stopped` already
   handles and which §8.2 already analyzes as the 500 case. Turning an unanalyzed
   failure into an analyzed one is the cheapest win here. The daemon's working set
   is a ~30 MB dataset plus a trie; sizing is DevOps's call.
2. **Add the row to §8.2** — "process alive, not answering" — and soften §1.7's
   and § Consequences' immunity claim to *immune to BitBlocker's decisions; shares
   Traefik's connection capacity*. The claim as written will be relied on.
3. **Distinguish liveness from readiness in §9's external check.** A TCP connect
   to the daemon that hangs or times out is a liveness failure (act: restart is
   appropriate, and unlike the `/healthz` case a restart genuinely may fix it); a
   `/healthz` 503 is a readiness failure (alert, do not restart). §9 currently
   collapses both into "poll `/healthz`, do not restart," which is right for the
   second and leaves the first unhandled.
4. **Confirm whether Traefik v3.6.13's `forwardAuth` exposes any timeout.** If it
   does, set it — a short one, since the daemon's decision is an O(1) trie lookup
   on a local network. If it does not, item 1 is the bound.

**Router risk: none.** Item 1 recreates the BitBlocker container only, and is
best done at Phase 3 when nothing references it.

---

### S-6 — the daemon sits on the shared `proxy` network, reachable by all 18 site containers

**Severity: medium. Non-blocking. Mitigation has a stated router cost — read the timing note.**

**Where.** `bitsalt/ansible#253` §5 ("Docker network membership: `proxy`
(external) only"); interface spec §2.1, which permits it.

**Why.** Every site container on app01 joins `proxy` (verified:
`roles/wordpress/templates/docker-compose.yml.j2` `networks: [proxy, internal]`).
Docker's user-defined bridge networks permit unrestricted container-to-container
traffic. Placing BitBlocker on `proxy` therefore makes its unauthenticated
`/check` and `/healthz` endpoints reachable from **all 18 site containers**, not
only from Traefik.

This is not a policy bypass — a compromised WordPress container calling `/check`
about itself learns nothing and changes nothing. The exposure is **availability**,
and it lands precisely where S-5 says it hurts: one compromised or merely
misbehaving site container can saturate the single shared daemon that 10 sites'
request paths now depend on, and per S-5 a saturated daemon degrades the shared
proxy for all 18. On a fleet that took a WordPress compromise on 2026-07-21, "a
compromised site container" is a demonstrated condition, not a hypothetical.

Interface spec §2.1 frames the reachability requirement entirely as
*not reachable from off-host*. The narrower and more useful property is
**reachable by Traefik and by nothing else**, which §2.1's own opening sentence
states and its prescribed mechanism does not deliver.

**Recommended mitigation.** Give BitBlocker a dedicated Docker network
(e.g. `bitblocker`) joined only by BitBlocker and Traefik, instead of `proxy`.
Traefik resolves the `forwardAuth` address by container name over that network;
BitBlocker carries no Traefik labels, so `traefik.docker.network` does not apply
and nothing about router configuration changes.

> **Router risk, stated plainly:** adding a network to Traefik's compose file
> **recreates the shared edge proxy**, which is a brief fleet-wide interruption —
> the class of event the operator's constraint is about. It is small and routine
> (the existing `Restart traefik` handler already runs `recreate: always` on
> ordinary dynamic-config changes), but it is not free.
>
> **Therefore: do this at Phase 0, in the same apply that renders
> `dynamic/bitblocker.yml`, before any router carries a label.** At that moment
> nothing references BitBlocker, the middleware is benign, and a Traefik recreate
> is the same routine event it is today. Doing it after Phase 2 means recreating
> the proxy while 10 sites' routers carry `bitblocker@file`, which is a strictly
> worse moment for the same change. If it is not done at Phase 0, my
> recommendation is to **not** do it later during this rollout — accept the
> shared-network exposure, record it, and revisit it at the next scheduled
> Traefik change. The finding is not worth an unscheduled proxy recreate.

---

### S-7 — the exemption set is trivially enumerable, and the control is a traffic-reduction measure rather than a containment boundary

**Severity: medium. Non-blocking. Affects how the control is described, and bears on OQ-B.**

**Where.** ADR 0005 §1.7, § Consequences ("The exemption is structural");
interface spec §8.2; OQ-B.

**Enumerability — the narrow question asked.** Yes, and completely. Once
enforcing, an attacker with an egress in any blocked country sends one request
per hostname: exempt sites answer 200, enforced sites answer the block status.
The hostname list is public — every site's certificate is in Certificate
Transparency, and all of them resolve to app01's single address. The exemption
set is discoverable in seconds with no privileged access.

One thing that does *not* leak: the `X-BitBlocker-Enforcement: disabled` header
is a **request** header, delivered to the backend, never to the client. It does
not help external enumeration. See S-9 for what it does disclose.

**Whether it matters — the part worth Jeff's attention.** Taken alone,
enumerability is close to harmless: an attacker who cannot reach 10 sites learns
which 8 he can, which he would learn in the same number of requests anyway. What
makes it worth a finding is the composition with the host's topology:

- All 18 site containers share the `proxy` network, with unrestricted
  container-to-container traffic (S-6).
- Eight site entries are unfiltered by design at Phase 4 (S-4): the two exempt
  hosting clients, five Node.js sites, one FastAPI service.
- They run the same roles, on the same host, behind the same proxy, with the same
  socket-proxy and the same fleet-wide autoheal.

An attacker whose goal is a foothold on app01 — which is the goal the 2026-07-21
compromise establishes as live — is not blocked by this control. He is handed a
precise, cheaply-obtained list of the eight entries that remain reachable, and a
foothold on any one of them puts him on the same network segment as the ten that
are "protected." Geo-filtering at the edge does not partition the host.

**This is not an argument against the control.** Dropping automated
edge-originated traffic from blocked countries against 10 WordPress sites is
worth having: it removes a large fraction of scanning, credential-stuffing and
xmlrpc/REST probing volume, and the `-login` attachment (§1.6) is genuinely the
highest-value part of it. It is an argument against describing it as containment.

**Recommended mitigation.**

1. Add a short subsection to ADR 0005 §3 or § Consequences stating what the
   control is and is not: *a reduction in edge-originated automated traffic
   against the attached sites; not a containment boundary, and not a reduction in
   lateral-movement surface on a shared host with a shared container network.*
   ADR 0005 § Consequences currently frames the exemption purely as an
   availability benefit ("structurally immune"); the residual security exposure
   is the other half of the same fact and is unstated.
2. **Carry this into OQ-B.** OQ-B asks whether BitSalt may drop 15 hosting
   clients' overseas visitors on their behalf. That is a cost/benefit question,
   and the benefit side of it is currently framed by documents that say "~16
   filtered sites" (S-4) and "structurally immune" (S-5, this finding). Jeff
   should answer OQ-B against the corrected framing: 10 sites, real traffic
   reduction, no containment guarantee, and a known list of overseas-visitor
   costs with no per-IP allowlist available to except anyone (OQ-3, still open).

**Router risk: none.** Documentation only.

---

### S-8 — the `-block-batch` exception is contingent on another middleware's literal sourcerange, with nothing binding the two together

**Severity: low. Non-blocking.**

**Where.** Interface spec §7.1 and §7.2; ADR 0005 §1.6.2;
`roles/wordpress/templates/docker-compose.yml.j2:94,157`.

**Verified.** The exception holds today. `{site}-block-batch` attaches
`{site}-block-batch`, defined at line 94 as
`ipallowlist.sourcerange=192.0.2.1/32` — a single TEST-NET-1 host that no real
client can present, so the router 403s unconditionally, and a `forwardAuth` call
in front of it has zero marginal effect. ADR 0005 §1.6.2's reasoning is correct
and the DoS argument for not attaching (this is the endpoint the 2026-07-21
attacker hammered; attaching would turn each attempt into a daemon round trip) is
a good one. **The exception should stand.**

**The gap.** §7.1 phrases the rule as "a router that already rejects every
request unconditionally," and §7.2's table records the exception. Neither binds
the exception to the fact that makes it true. If `{site}-block-batch`'s
sourcerange is ever widened — to let a legitimate integration through, which is
the ordinary reason anyone touches an `ipAllowList` — that router silently
becomes a live, geo-unfiltered path to the WordPress REST batch endpoint. The
change would be made in `roles/wordpress`, by someone working on an integration,
with no reason to read a BitBlocker interface spec.

This is the same "permissions decay" hazard ADR 0005 §1.6.1 invokes to justify
attaching the redirect routers, applied to the one exception it kept.

**Recommended mitigation.** Two lines, no behavior change. In interface spec
§7.2, state the exception's precondition explicitly: *conditional on
`{site}-block-batch`'s `ipAllowList.sourcerange` remaining a non-routable range;
if it is ever widened, attach `bitblocker@file` to that router in the same
change.* And put the reciprocal comment at
`roles/wordpress/templates/docker-compose.yml.j2:94`, next to the sourcerange —
where the person making the change will actually see it. The repo already uses
exactly this pattern for standing hazards (`dynamic.yml.j2:56-60`).

**Router risk: none.**

---

### S-9 — the benign body's header is client-forgeable in the enforcing rendering, and is disclosed to every backend

**Severity: low. Non-blocking. Contains a free improvement to V0b.**

**Where.** ADR 0005 §1.5; interface spec §6.3 and §10 (V0b).

**Is the benign body inert? Substantially yes — with two qualifications.** A
Traefik `headers` middleware carrying only `customRequestHeaders` sets one
request header and does nothing else: it does not alter routing, TLS, the
response, or the backend's view of the client address, and it adds no
security-header defaults. The claim that it is inert to WordPress, Node and
FastAPI holds. The design's choice of a benign body over an absent definition or
an empty chain is correct and §6.4's reasoning for it is right.

**Qualification 1 — the header is not a trustworthy enforcement-state signal.**
In the *disabled* rendering, `customRequestHeaders` **sets** the header, so any
client-supplied value of the same name is overwritten. In the *enforcing*
rendering, the middleware is a `forwardAuth` and the header is not set at all —
so a client may supply `X-BitBlocker-Enforcement: disabled` themselves and it
reaches the backend unmodified. The header therefore means "enforcement is off"
only for a request you control. That is fine for V0b, which is an operator
`curl`. It is not fine if anyone later builds monitoring, a dashboard, or an
alert on "backends are seeing the disabled header, so enforcement must be off" —
which is a natural thing to build, given §6.3 calls the header "positive evidence
in backend logs that enforcement is off."

The clean fix — stamp `X-BitBlocker-Enforcement: enforcing` in the enforcing
rendering too — requires a `chain` middleware plus a second definition, which
adds branches to a fragment §6.4 deliberately keeps small and loop-free, and a
parse error in that fragment is a fleet-wide router outage. **Not worth it.**
Instead, state the limitation in §6.3: *the header is authoritative only on a
request the operator originated; it is client-forgeable when enforcing and must
not be used as a monitoring signal.*

**Qualification 2 — free improvement to V0b.** Because the disabled rendering
*overwrites*, V0b can prove more than it currently does at no extra cost. Today
V0b confirms the backend receives the header — which proves the middleware ran,
unless something else on the path happens to add it. Send a bogus value and
confirm it was replaced:

```
curl -H 'X-BitBlocker-Enforcement: bogus' https://<pilot>/
# backend must log: X-BitBlocker-Enforcement: disabled
```

That distinguishes "the middleware ran" from "the header arrived" outright, and
it is the same single request V0b already makes.

**Qualification 3 — minor disclosure, no action required.** The header tells any
backend on an attached site whether fleet enforcement is on, and its absence
tells a backend that its site is exempt or unattached. Reachable via
`phpinfo()`, any plugin that logs request headers, or a compromised container.
The information is trivially obtainable from outside by other means, and an
attacker with backend code execution has larger levers. Recorded for
completeness; not worth mitigating.

**Router risk: none.**

---

### S-10 — the enforcement fragment must be written atomically, and nothing says so

**Severity: low. Non-blocking. Directly on the operator's stated fear.**

**Where.** Interface spec §6.1 and §6.4; ADR 0005 §1.5 and §5.3 lever 1.

**Why.** §6.4 establishes that an unparseable `dynamic/bitblocker.yml` causes the
file provider to log and skip it, removing the `bitblocker` middleware, which
puts every router referencing it into an error state — an outage of every
attached site. The spec treats this as a risk from a *typo*, and mitigates it by
keeping the template small and relying on lint.

There is a second path to the same state that lint cannot catch: a
**non-atomic write**. `--providers.file.watch=true` is set, so Traefik reacts to
the write immediately. Ansible's `template` and `copy` modules write to a
temporary file and rename, so the watcher never sees a partial file — but nothing
in the contract *requires* those modules. The incident lever (§5.3 lever 1) is a
two-line body flip, which is exactly the shape someone reaches for `lineinfile`,
`blockinfile`, `replace`, or a shell redirect to do — especially under incident
pressure, which is the only time lever 1 is used. Any of those can leave the file
momentarily truncated or syntactically incomplete, and the window lands on the
shared edge proxy while 10 sites' routers name the middleware.

The failure is transient and self-healing on the next write, which makes it worse
to diagnose, not better: routers drop for a few seconds during an incident
response, and the cause is invisible afterward.

**Recommended mitigation.** One line in interface spec §6.1's table:
*rendered atomically — `template` or `copy` only; never `lineinfile`,
`blockinfile`, `replace`, or a shell redirect.* Repeat it in §11 next to lever 1,
because that is where a hurried operator will be reading. Optionally, have the
render task validate the rendered YAML before it lands (`validate:` on the
`template` module), which closes the typo path too.

**Router risk: none.** It removes one.

---

### S-11 — `trustForwardHeader: true`'s stated rationale is probably inverted, and OQ-E's scoping follows from it

**Severity: info. Non-blocking.**

**Where.** Interface spec §6.2; ADR 0005 §1.5 and §4.2; `docs/traefik-integration.md:21`;
`bitsalt/ansible#253` §4's template comment; OQ-E.

**The claim.** All four documents assert that `trustForwardHeader: true` is what
causes Traefik to present the client address to the daemon, and that "without it
the daemon has no client address to decide on and fails closed on every request."
The ansible plan's inline comment sharpens it to "getting this one flag wrong
blocks 100% of traffic on every opted-in site."

**Why it is doubtful.** `trustForwardHeader` governs whether the *incoming*
`X-Forwarded-Method` / `-Proto` / `-Port` / `-Host` / `-Uri` are propagated to the
auth server versus re-derived from the request. The `X-Forwarded-For` presented
to the auth server is constructed by `forwardAuth` itself from the request's
peer address, appended to any prior chain — which is what makes the rightmost
entry the real client and is the basis for the daemon's rightmost-XFF policy.
If that is right, then with `trustForwardHeader: false` the daemon still receives
a usable `X-Forwarded-For`, the claimed 100%-block outcome does not occur, and
the flag's actual effect is to widen what client-influenced input reaches the
daemon.

**Why it does not change today's decision.** The daemon never reads
`X-Forwarded-Host` (`internal/server/server.go:216-278`, confirmed), so
propagating it costs nothing today, and setting the flag is not harmful. The
finding is about the *rationale*, and rationales get built on.

**What it does change.** OQ-E defers the empirical `X-Forwarded-Host` question as
blocking "§4.2 only, not this deployment," on the reasoning that
`trustForwardHeader` is the mechanism by which client-supplied headers might
reach the daemon. But S-1 shows a client-supplied header (`X-Real-IP`) reaching
the daemon by a path `trustForwardHeader` does not govern at all — so OQ-E's
framing understates the question's scope. The general form — *which
client-influenced headers reach `/check`, and by what mechanism* — is load-bearing
for **this** deployment, via `X-Real-IP`, today.

**Recommended mitigation.** Fold this into the V5 window (S-2): while the canary
country is enforced, dump the headers the daemon actually receives (temporarily
raise the daemon's log level, or point the middleware at a header-echo container
on the same network for one request). One observation settles
`trustForwardHeader`'s real effect, answers OQ-E outright, and confirms or
refutes S-1's stripping assumption. Then correct §6.2, `traefik-integration.md:21`,
and the ansible template comment to say what the flag does. If the flag proves
unnecessary, dropping it narrows the trust surface and pre-emptively unblocks
§4.2.

**Router risk: none**, provided the header-echo variant is run against the pilot
site only and reverted in the same session.

---

### S-12 — placing the geo check ahead of the login rate limiter removes the rate limiter's bound on daemon load

**Severity: info. Non-blocking. Accept, with a note.**

**Where.** Interface spec §7.3; `roles/wordpress/templates/docker-compose.yml.j2:76-78,138`.

The `-login` router's chain becomes `bitblocker@file,{site}-ratelimit-login`.
`{site}-ratelimit-login` is `average=5, burst=10, period=1m`. Putting the geo
check first means credential-stuffing traffic from a *non-blocked* country
generates one `/check` call per attempt at whatever rate the attacker chooses,
where previously the rate limiter capped what reached anything downstream. The
same holds, less sharply, on the apex router ahead of `-block-xmlrpc`.

**This is the right trade anyway.** §7.3's argument for first position is sound,
the per-request cost is a local HTTP round trip to an O(1) trie lookup, and a Go
server absorbs that at rates far above what this fleet sees. Reordering on
`-login` alone would reintroduce exactly the per-router judgment call §7.1
deliberately designed away. **Keep `bitblocker@file` first everywhere.**

**Worth recording:** the daemon is now the first hop for the *unrated* request
volume of 10 sites, and per S-5 its slowness is fleet-wide. That is a capacity
note for S-5's memory limit and for whatever `/check` latency monitoring gets
wired, not a reason to change the chain. §7.3 asserts a second benefit — "the
decision is never taken against a path some earlier middleware rewrote" — which
is true but inapplicable, since the daemon does not read the path at all; worth
striking so the rule's real justification stands on its own.

**Router risk: none.**

---

## Verdict on the fail-open posture

**Concur: keep `behavior.startup_mode: fail-closed` on app01. Do not set
`fail-open`.** ADR 0005 §3.6 reaches the right answer. I reach it by a different
primary route, and the difference matters for how the decision should be
defended later.

**The uncomfortable part of the case, stated first.** Under fail-closed with an
unusable blocklist, ten client sites return 403 to every visitor from every
country. Under fail-open they are unfiltered — which is the state they are in
today, and the state they will be in throughout Phases 0–3. So the failure mode
of fail-open is *"the control you did not have last month, you still do not
have,"* while the failure mode of fail-closed is *"ten paying clients' sites are
down."* That asymmetry is real and ADR 0005 §3.1 is right to insist on the
arithmetic rather than a posture preference.

**Why fail-closed still wins.** Two reasons, in order of weight.

**1. The posture knob is not where this deployment's availability risk lives, so
buying availability with it is a bad trade.** There are three ways attached sites
lose service to BitBlocker:

| Failure | Result | Governed by `startup_mode`? |
|---|---|---|
| Blocklist unusable | 403 to everyone (fail-closed) / unfiltered (fail-open) | **Yes** |
| Process down, crashed, OOM-killed, mid-pull, unreachable | Traefik 500 for every attached site | **No** |
| Process alive but not answering (S-5) | Requests hang; degrades all 18 sites | **No** |

Choosing `fail-open` buys protection against one row and leaves the other two —
including the worst one — fully exposed. It spends the entire purpose of the
daemon to insure against a third of the availability risk. The availability
answer on this host is the structural one ADR 0005 §3.6 already gives: Fix A
removes the common path into row 1, per-router attachment bounds who row 2
touches, the default-off switch gates when any of it is reachable, and the
restart-free kill switch makes row 1 and row 2 a ten-second file edit to undo.
Add S-5's memory limit and row 3 collapses into row 2. That set is worth more
than the knob, and it does not cost the control.

**2. Fail-open's failure is silent, on a host where the detection channel is not
yet wired.** ADR 0005 §3.4 makes this argument and it is correct. I would add one
thing it does not: the design's own §5.2 verification suite would not catch a
stuck fail-open either, for the same reason S-2 gives — every check goes green.
Detection collapses entirely onto the ERROR heartbeat, and OQ-G records that the
Loki alert on it does not exist. `fail-open` before OQ-G is closed is a control
that can silently stop working with no observer at all, on a fleet that has
in-repo comments documenting two controls that were inert in production for
months (`roles/wordpress/templates/docker-compose.yml.j2:122-134`, `:140-153`).

**On the question as posed — is fail-closed right for a control whose purpose is
post-compromise hardening?** Yes, and S-7 sharpens why. Precisely *because* this
control is traffic reduction rather than containment, its value is entirely in
being continuously, verifiably on. A containment boundary might justify a
graceful-degradation posture, since other layers would still hold. A
noise-reduction filter that silently stops filtering has no residual value at all
— it is purely decorative, which is the outcome this fleet has already paid for
once.

**Two conditions on the concurrence.** Neither is a new demand; both are already
in ADR 0005 and I am marking them as load-bearing for this verdict specifically:

1. **Fix A lands before Phase 4** (ADR 0005 §3.5, §3.6). The finding at §3.3 —
   that `cache.max_age` is inoperative in the steady state because a 304 never
   advances the cache mtime, and a stale cache is deleted rather than skipped —
   is the reason fail-closed's exposure window is currently much wider than ADR
   0004 §E.2 costed. Fail-closed is the right posture *with* the guardrail
   working. Without Fix A, the "16 sites down" scenario (10, per S-4) is reachable
   by any routine restart that coincides with a DB-IP fetch failure, and the
   concurrence above assumes it is not.
2. **OQ-G is closed before Phase 4.** Fail-closed's failure is loud to visitors
   and therefore self-detecting, so this is a weaker precondition than it is for
   fail-open — but the whole argument against fail-open is that silent failure is
   unacceptable, and it is not consistent to lean on that while shipping without
   the alert that makes any failure legible.

**Not reopened:** ADR 0004's rejection of time-bounded fail-open. It is correct
and the shared-host framing does not change it.

---

## On whether this surface warrants a threat model

**No separate `threat-model.md` for this change.** The trust surface is small and
fully enumerated by this review: one unauthenticated HTTP endpoint on an internal
network, one attacker-influenced input class (the client-IP headers, S-1 and
S-11), one policy-selection mechanism that lives entirely in static
configuration, and an availability surface (S-5, S-6, S-12). A STRIDE pass over
that would restate the findings above with more ceremony and no new content.

**One surface does warrant one, before it enters a sprint:** ADR 0005 §4.2's
`X-Forwarded-Host` policy selection. That feature moves the exemption decision
*inside* the daemon, keyed on a header, and its correctness then depends on
propagation behavior that S-11 shows is currently misunderstood in four
documents. §4.2 already blocks itself on an empirical determination, which is
right. Add: if that determination comes back favorable and the feature is
scheduled, it gets a `THREAT-MODEL` pass first — the failure mode there is a
total bypass reachable by any client with a header, which is a materially
different risk class from anything in the current design.

---

## Handoff

**Blocking for the handoff (3):**

| # | Finding | Owner |
|---|---|---|
| S-1 | `X-Real-IP` trust contract unstated and unenforced | Architect (spec text) + DevOps (traefik comment) + Developer (v1.1 header policy) |
| S-2 | Verification cannot detect a header-spoofing bypass — add V5 | Architect (spec text) + DevOps (run at Phase 4) |
| S-3 | `/healthz` assert vs. no-published-port contradiction | Architect (decide) + DevOps (implement) |

**Non-blocking, close before Phase 4 (3):** S-5 (hang failure mode + memory
limit), S-7 (control framing, feeds OQ-B), S-10 (atomic render).

**Non-blocking, documentation (4):** S-4 (arithmetic), S-8 (`-block-batch`
precondition), S-9 (header caveat + free V0b improvement), S-11/S-12 (rationale
corrections).

**S-6 is conditional on timing** — take it at Phase 0 or not during this rollout.

**No finding requires a code change to the shipped binary before Phase 4.** Fix A
(ADR 0005 §3.5) already did, independently of this review; S-1 item 3 is a v1.1
follow-on.

## Needs an operator decision

1. **S-3** — whether `/check` is published on `127.0.0.1:8080` (permitting the
   `/healthz` pre-flight assert and the host-side poll as the ansible plan
   assumes) or stays unpublished with checks run inside the `proxy` network.
   Recommendation: permit loopback publishing explicitly, forbid every other
   form.
2. **S-6** — whether to take the dedicated-network change at Phase 0, accepting
   one scheduled Traefik container recreate while nothing references BitBlocker.
   Recommendation: yes at Phase 0; otherwise defer past this rollout entirely.
3. **S-7 into OQ-B** — OQ-B should be answered against the corrected picture: 10
   filtered sites of 18 entries, real reduction in automated edge traffic, no
   containment guarantee, no per-IP allowlist to except a named overseas address
   (OQ-3 open). This does not change the question; it changes the benefit side of
   it.
