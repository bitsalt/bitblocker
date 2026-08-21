# ADR 0005: On a shared multi-tenant host, policy is selected by per-router middleware attachment, never at the entrypoint; enforcement is a separate fleet switch that defaults off; `fail-closed` stays, and the disk-cache guardrail is repaired rather than the posture flipped

- **Status:** proposed
- **Date:** 2026-08-03
- **What unblocks promotion:** `plan-acceptance-gate` — Jeff's acceptance of the app01 BitBlocker deployment plan, of which this ADR is the architecture half. Two operator inputs named in § Open questions (the `block.countries` content, and whether the 15 non-exempt hosting clients have been told their overseas traffic will be dropped) are inputs to that same gate.
- **Deciders:** Architect — the attachment model, the enforcement-switch shape, the intermediate-state analysis, the fail-mode analysis, the cache-defect finding, and the growth path. DevOps — the ansible-side realization (task shape, variable plumbing, run mode). Jeff (operator) — plan acceptance, the country list, and the client-communication question.
- **Reconciles:** `bitsalt/bitblocker#27` (the original of this ADR) and `bitsalt/ansible#253` (`devops/ansible/bitblocker-deployment-plan`). Two designs for the same per-site contract were produced concurrently by Architect and DevOps, neither aware of the other. See § Reconciliation. The settled contract below replaces both; § What `bitsalt/ansible#253` must change states the ansible-side delta.
- **Amends:** ADR 0004 §E.2 — a factual correction to its claim that the disk cache covers the realistic restart-during-outage overlap. See §3.3 below; the correction block is filed in place in ADR 0004.
- **Interacts with:** ADR 0002 (disk-cache snapshot — §3 depends on its staleness bound and its removal-on-stale behavior), ADR 0003 (the keyless DB-IP fetch that §3's threat path runs through), ADR 0004 (the fail-open readiness gate and its observability contract — unchanged by this ADR except for the §E.2 correction). In `bitsalt/ansible`: ADR 0020 (file-provider layout — §1.5 requires a narrow, stated deviation from its split-by-kind rule), ADR 0005 there (per-site named dict vars — the convention §1.3's key follows).
- **Supersedes:** none
- **Superseded by:** none

## Context

BitBlocker is a shipped v1.0 tool with no deployment anywhere in the estate
(`docs/deployment.md` opening paragraph). The first deployment target is
**app01**, a single DigitalOcean host running a fleet of sites behind one shared
Traefik v3.6.13 instance (`bitsalt/ansible`, `roles/traefik/defaults/main.yml:14`).

Verified against `bitsalt/ansible` at `site.yml:89-141` on 2026-08-03, the fleet
is **18 site entries across three role families plus umami**:

| Family | Count | Role | Router shape |
|---|---|---|---|
| WordPress | 12 | `roles/wordpress` | Multiple routers per site (apex, `-login`, `-block-batch`, `-www`, `-alias-N`, `-alias-N-www`) |
| Node.js | 5 | `roles/webapp` | Single router per site; **no `middlewares=` label today** |
| FastAPI | 1 (`portfolio-service`) | `roles/webapp` | Single router; **no `middlewares=` label today**; carries the `/internal/` GitHub-webhook route |
| Analytics | 1 (`umami`) | `roles/umami` | Single router, `middlewares=hsts-headers@file`; uses the **HTTP-01** ACME resolver `le-http` |

The driving requirement is **binary and per-site**: most hosted sites should get
country blocking; **two hosting clients specifically need outside-the-U.S.
traffic to reach their sites** and must be exempt.

### What the daemon can and cannot do today

Verified against the bitblocker working tree at `main` on 2026-08-03:

- **`block.countries` is a single flat global list** (`internal/config/config.go:72-75`).
  One daemon process carries exactly one policy.
- **`/check` decides from the client IP alone.** `handleCheck`
  (`internal/server/server.go:216-278`) reads only `X-Real-IP` and the rightmost
  parseable `X-Forwarded-For` entry (`internal/server/clientip.go`). Traefik's
  `forwardAuth` sends `X-Forwarded-Host`, but **BitBlocker never reads it** —
  there is no per-host policy selection.
- **Therefore all per-site selection must happen in Traefik**, by choosing which
  routers call `/check` at all.

### The operator's constraint, which is an acceptance condition on this design

> "the Traefik routers all going down when I can't currently count on my system
> is not a good place to be... tread lightly."

Read as an acceptance condition, not a caveat: **the state where the ansible
template has landed but the BitBlocker daemon does not yet exist must be
harmless.** A design that is correct only once everything is deployed in the
right order does not satisfy it. §2 is the section that discharges this
obligation, and it is the section to read first if you are reading only one.

### The four things this ADR settles

- **A.** The contract — the per-site declaration, the fleet enforcement switch,
  the middleware definition, and where it attaches. §1.
- **B.** The intermediate state — what is true between "nothing deployed" and
  "fully deployed," and why every step of it is harmless. §2.
- **C.** The fail-mode posture on a shared host, where one unusable blocklist is
  a multi-tenant event rather than a single-site one. §3.
- **D.** Whether app01 needs more than one policy now, and the growth path if it
  later does. §4.

Rollout, blast radius, and the rollback levers are §5. The Traefik-side contract
DevOps builds against is `docs/interfaces/traefik-middleware-attachment.md`; the
Developer-side contract for the §3 code changes is
`docs/interfaces/cache-freshness-and-stale-fallback.md`. This ADR is the
decision; those are the specs.

---

## Reconciliation

A planning pass launched Architect and DevOps concurrently rather than in
sequence. Each independently designed the same per-site contract; two PRs are
now open against incompatible designs. **Both caught a real hazard. Neither is
simply wrong, and neither is complete.**

### The two designs, as they stood

| | `bitsalt/bitblocker#27` (Architect) | `bitsalt/ansible#253` (DevOps) |
|---|---|---|
| Per-site key | `geo_filter`, string enum `enforce`\|`exempt` | `bitblocker_country_block_enabled`, optional boolean, declared in `roles/wordpress/meta/argument_specs.yml` under `item.options` |
| Per-site default | **None — required key**, play fails on omission | **`true`** |
| Fleet-level control | `bitblocker_enforcing` (bool, **default `true`** — wrong, see below), selecting between two renderings of the middleware | `bitblocker_enabled` (bool), **already shipping `false`** in `group_vars/all/vars.yml` |
| What gates a router label | The per-site key alone | **Both switches**, in the template: `{% set item_bitblocker_enabled = (bitblocker_enabled \| default(false) \| bool) and (item.bitblocker_country_block_enabled \| default(...)) %}` |
| Routers attached | Apex and `-login` (2) | Apex, `-login`, `-www`, `-alias-N`, `-alias-N-www` (5); `-block-batch` deliberately skipped |
| Stated reason | No default is safe: a new site must neither silently acquire nor silently lose a policy; and a required key fails at apply time rather than at traffic time. | Default-`true` is fail-safe by asymmetry: a new site added without the flag is blocked and fails *loudly*, whereas default-`false` leaves it silently unprotected. The fleet switch, shipping `false`, keeps the landing moment safe. |

### What each got right

**DevOps got the layering right, and got the default right — and had already
shipped it right.** The insight that a per-site flag *alone* cannot make the
landing moment safe is correct and it is the more important of the two findings.
A per-site declaration answers "should this site be filtered"; it cannot answer
"does the thing that filters exist yet." Those are two different questions and
they need two different switches.

**On the fleet switch's default, the error was Architect's alone.**
`bitblocker_enabled` ships `false` in `group_vars/all/vars.yml` on the DevOps
branch, stated there as the state the PR ships in. Architect's
`bitblocker_enforcing` defaulted `true`. A fleet switch that points a security
control at a daemon which may not exist must default **off**; DevOps had this
right from the start and this ADR is correcting Architect, not DevOps.

**DevOps also got the router coverage right**, and more right than Architect —
see §1.6, where the five-router attachment is adopted over Architect's two.

**Architect got the key's shape right**, though for partly the wrong reasons —
see §1.3, where the rationale is re-ranked now that the real declaration
(`argument_specs.yml`, typed) is known. The durable advantages are that a
required key makes "considered and chosen" distinguishable from "nobody thought
about it," and that it fails at apply time rather than at traffic time.
Architect also had the non-negotiable invariant that the middleware must be
**defined in every rendering** (§1.5) — a Traefik router referencing a
middleware that does not exist stops serving, so "disable by deleting the
middleware" is an outage, not a kill switch.

### What neither had

Three things, surfaced by reconciling them against the live ansible repo:

1. **Architect's "10-second, restart-free kill switch" does not work as
   specified.** The task that renders `dynamic/middlewares.yml` carries
   `notify: Restart traefik` (`roles/traefik/tasks/main.yml:48-55`), and that
   handler runs `recreate: always` (ansible ADR 0020 § References,
   `docs/ops/ansible.md` § Container recreation policy). Flipping enforcement by
   re-rendering that file therefore **recreates the shared edge proxy for all 18
   sites** — precisely the fleet-wide blip the operator's constraint is about.
   §1.5 fixes this by giving the middleware its own file-provider fragment with
   no notify.
2. **Attachment and enforcement were conflated.** Adding a `middlewares=` label
   to a site rewrites its compose file and recreates its container — minutes of
   work across 12 sites, with a brief per-site interruption, and the same cost
   again to reverse. Flipping a file-provider fragment is seconds and reverses in
   seconds. The settled contract puts the **slow, disruptive, hard-to-reverse**
   step (labels) where it is *inert*, and the **fast, restart-free, instantly
   reversible** step (the fragment's body) where the dangerous behavior lives.
   Neither original design made that split. DevOps' is explicit about it — the
   template's `item_bitblocker_enabled` is the logical AND of the fleet switch
   and the per-site flag, and it gates **label emission** — so on that branch as
   it stands, turning enforcement off is a 12-container recreate and turning it
   back on is another. §1.2 is the correction.
3. **Nothing checked that the daemon was actually answering before enforcement
   was rendered.** §1.4 adds a pre-flight `/healthz` assert.

### The settled contract, in one paragraph

Three independent things, each with one job. **`geo_filter`** (required string
enum, per site) declares whether a site's terminal routers carry the middleware
— a *static* statement, always emitted, inert on its own.
**`bitblocker_enforcing`** (fleet boolean, **default `false`**) selects which of
two bodies the middleware named `bitblocker` is rendered with — a benign
request-header stamp, or the real `forwardAuth`. And **the middleware named
`bitblocker` is defined at all times**, on every host where any router might
reference it, so referencing it is never an error. A request reaches the daemon
only when both `geo_filter: enforce` **and** `bitblocker_enforcing: true` hold;
until the second is deliberately flipped — behind a pre-flight check that the
daemon answers `/healthz` — the whole apparatus is a no-op that stamps a header.

This keeps DevOps' two-level structure and DevOps' default-off posture, keeps
Architect's required-enum key and always-defined invariant, and adds the
static-attachment / dynamic-enforcement split and the pre-flight check that
neither had.

### Provenance of the DevOps side, and what changed once the diff was read

The first draft of this ADR was written without the `bitsalt/ansible#253` diff
(Architect has no Bash; the diff had not been supplied) and reconciled against a
characterization of it. The diff was subsequently read and supplied. Recorded
here because three claims in that first draft were wrong, and the corrections
matter more than the fact of the error:

| First draft said | The diff shows |
|---|---|
| The fleet switch is named `bitblocker_country_block_enabled` | That is the **per-site** flag. The fleet switch is `bitblocker_enabled`. |
| Architect was correcting DevOps' fleet-switch default | **DevOps already ships `false`.** The bad default was Architect's own. |
| DevOps attaches to the same routers Architect named | DevOps attaches to **five** routers; Architect named two. DevOps is right — §1.6 adopts the five. |

Two claims were confirmed rather than corrected: the per-site key really is an
optional boolean (so §1.3's critique applies to the real design, in its re-ranked
form), and DevOps really does gate **label emission** on both switches (so §1.2's
correction applies to the real design).

Everything asserted about the ansible repo's task shape, role ordering, handler
behavior, and router lines was read from that repo directly and carries file:line
citations; none of it depended on the diff.

**OQ-I, which tracked this gap, is resolved (2026-08-03).** No part of the
settled contract was overturned by reading the diff; §1.3's *rationale* was
re-ranked and §1.6's *coverage* was widened.

---

## Decision

### 1. The contract: a static per-site declaration, a fleet enforcement switch, and a middleware that always exists

#### 1.1 The three parts at a glance

| Name | Level | Type | Default | Gates | Cost to change |
|---|---|---|---|---|---|
| `geo_filter` | Site dict, `vars/sites/<site>.yml` | String enum `enforce` \| `exempt` | **None — required** | Whether that site's terminal routers emit `bitblocker@file` | Container recreate for that site (~seconds of interruption); `-e wordpress_site_filter=<site>` scopes it to one site |
| `bitblocker_enforcing` | Fleet, `group_vars` | Boolean | **`false`** | Which body the `bitblocker` middleware is rendered with | One small file rewrite, picked up by Traefik's file watcher; **nothing restarted** (§1.5) |
| `bitblocker_address` | Fleet, `group_vars` | String | None | The `forwardAuth` target. Required — asserted — when `bitblocker_enforcing` is true | Same as above |

**The invariant that ties them together, stated as a hard rule:**

> **The middleware named `bitblocker` is defined on every host where any router
> may reference it, in every rendering, unconditionally — including when
> `bitblocker_enforcing` is false and including when no BitBlocker daemon exists
> on the host at all.**

This is what makes a `bitblocker@file` label safe to carry at any time, which is
in turn what makes attachment static and enforcement dynamic, which is in turn
what makes §2's intermediate state harmless. Removing this invariant collapses
the whole design.

#### 1.2 Why the split is the load-bearing idea

Each site dict already carries a per-site `enabled`; the temptation is to model
geo-filtering the same way, as one flag that means both "this site is filtered"
and "filtering is on." Resist it. The two facts have different lifetimes,
different blast radii, and different reversal costs:

- **"Which sites would be filtered"** changes rarely, is a per-client
  commitment, and changing it costs a container recreate on the affected site.
- **"Is filtering happening right now"** is an *incident-time* fact. It must be
  flippable in seconds, must not restart anything, and must be reversible by the
  same motion.

Collapsing them into one switch means every incident-time flip is a 12-container
recreate on the shared host — slow to apply, slow to undo, and touching every
tenant. Keeping them separate means the incident lever touches one small file
and no container.

#### 1.3 The per-site declaration: `geo_filter`, required, a string enum

Each site dict in `bitsalt/ansible` `vars/sites/<site>.yml` gains **one required
key**:

```yaml
abitwiser_site:
  enabled: true
  site_name: abitwiser
  geo_filter: enforce        # required; "enforce" | "exempt"
```

**`geo_filter` has no default and the play asserts its presence and its value**,
unconditionally — not gated on `bitblocker_enforcing`. A site dict that omits
it, or carries anything other than the two literal strings, fails the play with
a message naming the site and both legal values.

**Why unconditional rather than gated on the fleet switch.** The key's job is to
make the enumeration explicit. A site added while enforcement is off would
otherwise silently join the enforce set the moment enforcement is flipped on —
which is exactly the "silently acquires a policy" failure the required key
exists to prevent, merely deferred. Declaration is always required; only
*behavior* is gated.

**Why no default — stated against DevOps' argument, not around it.**
`bitsalt/ansible#253` §3 argues for `default: true` from asymmetric failure, and
the argument is good enough that it has to be answered directly. Paraphrased: a
new site added without the flag is, under default-`true`, blocked-by-default —
it fails **loudly** (403s, a ticket, one bool flipped) — whereas under
default-`false` it is silently unprotected, "exactly the class of gap this whole
feature exists to close," and default-`true` matches the fleet's
post-compromise hardening posture.

**That argument is correct, and it defeats default-`false` completely.** Between
the two defaults, `true` is the right one, for exactly DevOps' reason. Nothing
below disputes it.

What it does not establish is that a *default* is the right instrument, because
the comparison it runs is only between two defaults. Run it across all three
options and the axis that matters is **when the failure surfaces**:

| Option | Failure on an omitted flag | When it surfaces | Who detects it | Cost at detection |
|---|---|---|---|---|
| Default `false` | Site silently unprotected | Never | Nobody | The gap this feature exists to close |
| Default `true` | Site blocked from every listed country | **Traffic time** | A visitor, or the client, hitting a 403 | A real outage on a real client's site, however brief, plus a support ticket |
| **Required key** | Play fails | **Apply time, before any change is made** | The operator running the apply | **Nothing.** No visitor sees anything, no container is recreated, no 403 is served |

Default-`true` buys loudness by spending a client-visible outage. The required
key buys the same loudness for free, because it fires before anything on the
host has changed. Both are fail-safe; one of them is fail-safe without a bill.
Precedent for preferring the apply-time failure in the same repo:
`roles/traefik/tasks/main.yml:98-107` deliberately keeps the DNS-01 credential
task failing loudly rather than defaulting, "so a genuine future loss of the
token is never silently skipped."

**And on this fleet specifically, default-`true` has a second cost DevOps' §3
does not price.** These are *client* sites. A new site deployed without the flag
would, under default-`true`, have its clients' overseas visitors dropped from
the moment of deployment until someone complains — and OQ-B is the still-open
question of whether BitSalt may make that call on a hosting client's behalf at
all. A default that answers an unresolved product question by omission is worse
than a key that forces whoever adds the site to answer it. The required key
converts a silent product decision into an explicit one.

**The residual, conceded:** a required key makes omission loud but does not
*steer* the person adding site #19 toward `enforce`. DevOps' default does. This
is the one property their design has that this one does not, and it is real. The
mitigations are weaker than a default: the assert message names the recommended
value ("use `enforce` unless the site has a documented exemption"), and the
greppable exemption set makes a carelessly-added `exempt` visible in review.

**A string enum, not a boolean** — re-ranked now that the real declaration is
known. `bitblocker_country_block_enabled` is declared in
`roles/wordpress/meta/argument_specs.yml` under `item.options`, which changes the
weight of these three reasons:

1. **Omission is distinguishable from declaration.** With a defaulted boolean,
   "this site was considered and chosen for enforcement" and "nobody thought
   about this site" are *byte-identical config*. No argument-spec default can
   restore that distinction, and it is the whole point of the key. **This is the
   load-bearing reason.**
2. **The exemption set is auditable.**
   `grep -c 'geo_filter: exempt' vars/sites/*.yml` is an exact count. A defaulted
   boolean's exemption set is the `false`-valued entries *plus every site relying
   on the default* — which is not greppable at all, and which is the set OQ-B
   will eventually need enumerated.
3. **YAML boolean coercion — demoted to third, and narrowed.** If the argument
   spec declares `type: bool` (as the key's name implies), ansible's own
   validation rejects an unparseable typo, so the strong form of this objection —
   `ture` silently evaluating falsy and disabling the control — does not survive
   contact with the real declaration. What remains is legibility, not safety:
   YAML has several spellings of false (`no`, `off`, `n`), and a reviewer
   scanning vars files reads `exempt` more reliably than `no`. That is worth
   something, but it is not worth what the first draft of this ADR claimed for
   it.

#### 1.4 The fleet enforcement switch: `bitblocker_enforcing`, default `false`

One fleet-level boolean selects which body the `bitblocker` middleware carries.
**Its role default is `false`** and it is set to `true` explicitly in
`group_vars` for the production host, once and deliberately.

**The default is DevOps', unchanged.** `bitblocker_enabled` already ships
`false` in `group_vars/all/vars.yml` on `bitsalt/ansible#253`. A security
control whose middleware points at a daemon that may not exist must default to
not pointing at it, and DevOps had that right from the start. The variable that
defaulted `true` was Architect's `bitblocker_enforcing`, and it was wrong in
exactly the direction the operator named.

**On the rename from `bitblocker_enabled`.** The rename is not preference and
not tidying — **the variable's meaning changes**, so carrying its old name
forward would be the more dangerous option. On the DevOps branch,
`bitblocker_enabled` is one conjunct of `item_bitblocker_enabled` and therefore
gates **label emission**; under this contract the fleet switch selects the
middleware's *body* and never touches a label (§1.2). A reader who knows the old
variable would carry the old semantics into the new template. A second reason,
weaker but real: `..._enabled` invites the reading "is BitBlocker installed on
this host," which is a separate, DevOps-owned concern this contract deliberately
does not cover (interface spec §2.5).

**Two pre-flight asserts run when `bitblocker_enforcing` is true**, before the
enforcing body is rendered:

1. `bitblocker_address` is defined and non-empty.
2. `GET http://{{ bitblocker_address }}/healthz` returns **200**. A 200 means the
   daemon is up *and* has a usable blocklist (ADR 0004 §C); a 503 means it is up
   but unusable, and rendering the enforcing form against a 503 daemon would
   fail-closed every attached site.

These asserts carry their **own tag** so an operator can deliberately
`--skip-tags` them for one apply — the same shape and the same reasoning as the
DNS-01 credentials task at `roles/traefik/tasks/main.yml:98-107`, which fails
loudly by default and is skippable only as an explicit one-apply opt-in. Without
the skip tag, an unrelated apply during a BitBlocker outage would be blocked by
this assert, which is its own "tread lightly" violation. With it, the default is
loud and the escape hatch is explicit.

#### 1.5 The middleware: defined once, always, in its own file-provider fragment

The `forwardAuth` middleware is defined **exactly once**, in Traefik's file
provider (`{{ traefik_proxy_dir }}/dynamic/`, rendered by `roles/traefik`),
named `bitblocker`, and referenced from routers as **`bitblocker@file`**. It is
*not* defined via docker labels on the daemon's own container — that would put
the kill switch behind a container recreate and couple the middleware's
existence to the lifecycle of the very thing being toggled.

**It gets its own fragment — `dynamic/bitblocker.yml` — not a stanza inside
`middlewares.yml`, and its render task does not notify `Restart traefik`.**

This is a deliberate, narrow deviation from `bitsalt/ansible` ADR 0020, which
mandates splitting file-provider config by *kind* (`middlewares.yml`,
`routers.yml`, `tls.yml`) rather than by concern. The reason is mechanical and
decisive: the existing task at `roles/traefik/tasks/main.yml:48-55` renders
`middlewares.yml` **with `notify: Restart traefik`**, and that handler runs
`recreate: always`. Putting the `bitblocker` middleware in `middlewares.yml`
therefore means every enforcement flip recreates the shared edge proxy for all
18 sites. The entire value of this lever is that it is fast and restart-free;
inheriting that notify destroys it.

`--providers.file.watch=true` is already set
(`roles/traefik/templates/docker-compose.yml.j2:40`), so a content change to a
fragment in that directory is picked up within seconds with **no container
restarted and no router label changed**.

Two consequences of the separate fragment, stated so they are not discovered
later:

- **It requires an amendment to `bitsalt/ansible` ADR 0020.** That ADR's
  "Reconsider if" already anticipates a per-concern split; this is the first case
  that forces it, and it forces it for a reason ADR 0020 did not consider (the
  notify's blast radius). Filing that amendment is ansible-side work — see
  § What `bitsalt/ansible#253` must change, item 4, and OQ-J.
- **A YAML parse error in the fragment silently removes the middleware.** The
  file provider logs and skips an unparseable file rather than failing the
  container (ansible ADR 0020 § Consequences), and a removed `bitblocker`
  middleware puts every attached router into an error state. This hazard is not
  *created* by the separate fragment — a typo in `middlewares.yml` would remove
  it too — but isolating it into a small, loop-free, two-branch template is the
  mitigation, together with the repo's existing yamllint/ansible-lint pass.

**Enforcing body:**

```yaml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://<bitblocker_address>/check"
        trustForwardHeader: true
```

**Non-enforcing body (the default, and the shape that ships first):**

```yaml
http:
  middlewares:
    bitblocker:
      headers:
        customRequestHeaders:
          X-BitBlocker-Enforcement: "disabled"
```

The non-enforcing body is a benign pass-through, **never an absent definition
and never an empty `chain`**. The added request header is inert to WordPress,
Node, and FastAPI, and it is deliberately not nothing: it is the observable
signal that lets §5.2's V0b *prove* attachment-while-disabled is harmless rather
than assume it. There is direct precedent in this repo for the failure this
avoids — `roles/traefik/templates/dynamic.yml.j2:56-60` carries a standing
warning that `asana-ip-allowlist` is emitted only when non-empty and must
therefore never be referenced from a router label, because a referenced-but-
undefined middleware breaks the router.

`trustForwardHeader: true` is required, not cosmetic: it is what causes Traefik
to present the `X-Forwarded-For` / `X-Real-IP` it derived from the real TCP peer
(`docs/traefik-integration.md` § Register the middleware). Without it the daemon
has no client address to decide on and fails closed on every request.

#### 1.6 Which routers carry it

**The settled rule — DevOps' coverage, adopted over Architect's:**

> **For a site with `geo_filter: enforce`, every router the site role emits
> carries `bitblocker@file`, with exactly one exception: a router that already
> rejects every request unconditionally, where the middleware has no marginal
> effect. Routers serving machine traffic are never attached; that list (§1.6.3)
> is a property of the surface, not of the site's flag.**

For a WordPress site today that is **five routers**: apex (`:121`), `-login`
(`:138`), `-www` (`:185`), `-alias-N` (`:201`), and `-alias-N-www` (`:229`).
The exception is `-block-batch` (`:157`).

The `-login` router remains the one that must not be missed: its rule is
`Host(domain) && Path(/wp-login.php)`
(`roles/wordpress/templates/docker-compose.yml.j2:135`) and it has *higher*
match priority than the apex router, so attaching only to the apex leaves the
single highest-value target on a WordPress site — the login form — reachable
from every blocked country. This is not a hypothetical class of error in this
repo: the comments at `:122-134` and `:140-153` both document a control that was
inert in production for months because a per-router attribute was missing on
exactly these two routers.

##### 1.6.1 The redirect-router question, decided

Architect's original rule attached only to routers that "can terminate at a
site's own backend," leaving `-www` and the alias routers unattached on the
grounds that a blocked visitor following the 301 lands on the apex and is blocked
there — one wasted round trip, no bypass. DevOps attaches to them, reasoning:
"Bitblocker goes **first** in each chain (block before any other middleware runs,
including a redirect — a blocked visitor shouldn't get a 301 first)."

**DevOps' coverage is adopted.** The bypass analysis in Architect's version is
correct and is not the reason:

- **There is no bypass either way.** The redirect-only routers never serve
  content. `-www` redirects to the apex, which is attached. The alias routers
  redirect to `alias.to_url` (`:204`, `:232`), which is arbitrary — but since
  they only ever emit a 301, a blocked visitor cannot obtain anything from them
  regardless of policy.
- **Attaching does not endanger certificates.** The `-www` and alias routers
  carry `tls.certresolver=le` — *standalone DNS-01* orders (`:160-181`,
  `:207-223`), not HTTP-01 — so no inbound HTTP validation passes through these
  routers and geo-filtering them cannot break renewal. This was the objection
  worth checking before adopting, and it does not hold. The HTTP-01 concern is
  real but lives elsewhere, on the `web` entrypoint, and is unaffected (§1.6.3).

The decision turns on two things Architect's framing missed:

1. **The rule gets simpler, and simple rules drift less.** "Every router except
   ones that already reject everything" needs no per-router judgment; "every
   router whose rule can terminate at the backend" needs a judgment call each
   time a router is added, and § Consequences already names per-router drift as
   the accepted cost of rejecting entrypoint attachment. Widening the coverage
   shrinks the surface where that cost is paid. It also cannot under-attach,
   which is the failure direction that matters.
2. **"May omit" is a permission, not a decision, and permissions decay.** A rule
   that says redirect routers *may* be skipped will eventually be read as
   licence to skip a router that turns out not to be redirect-only.

DevOps' own stated reason is weaker than these but is not wrong: serving a 301 to
a visitor you have already decided not to serve is doing work on their behalf,
and it costs a round trip on every blocked request to a `www.` or alias
hostname. Small, but on the right side.

**What adopting it costs, stated:** three more labels per site, three more
`/check` calls per redirect-path request, and a slightly wider surface that a
daemon failure touches — `www.<site>` would 500 rather than 301-to-a-500-apex.
That last is nil in practice, since the redirect's target is already down in
that scenario. One genuine oddity: an alias whose `to_url` points off-host now
has its *redirect* geo-filtered even though the destination is not ours. Harmless,
and not worth a per-alias exception.

##### 1.6.2 The one exception, and why aliases have no key of their own

`-block-batch` (`:154-158`) is not attached. It already returns 403 to every
request via a TEST-NET-1 `ipAllowList` sourcerange, so a `forwardAuth` call in
front of an unconditional rejection has zero marginal effect — it adds a network
round trip to the daemon on every request that was going to be refused anyway,
and adds the daemon's availability to that router's dependency set for no gain.
This is DevOps' reasoning and it is also the general form of the exception in
the rule above.

**Aliases inherit the parent site's `geo_filter`; there is no per-alias key.**
Adopted from DevOps, whose reasoning — scope creep against a need that does not
exist — is right. An alias is a parked domain redirecting into the site; a
policy that differed between a site and its own alias would be a distinction
without a case behind it. If one ever appears, it is an additive change to the
alias dict, not a reshaping of this contract.

##### 1.6.3 Routers that must never carry it

These serve machine traffic that does not originate where a human visitor does.
This list is not a site-flag question — it holds regardless of any site's
`geo_filter` — and it is unaffected by §1.6.1's widening:

- **The `web` entrypoint / ACME HTTP-01 path.** The `le-http` resolver
  (`roles/traefik/templates/docker-compose.yml.j2:65-68`) validates over port 80
  from Let's Encrypt's vantage points, which are multi-regional by design.
  Geo-filtering that path breaks certificate renewal — and breaks it silently,
  60 days before anyone notices. The DNS-01 resolver `le` is unaffected (no
  inbound HTTP).
- **`portfolio-service`'s `/internal/` webhook routes.** GitHub's webhook
  egress is already constrained by an `ipAllowList` (`internal-ip-allowlist`);
  layering a geo filter over it adds a second, less legible failure mode for a
  push-triggered sync.
- **Any uptime monitor or external health probe path** — see § Open questions.

#### 1.7 Never at the entrypoint — and this is the load-bearing negative

The all-sites lever exists
(`--entrypoints.websecure.http.middlewares=hsts-headers@file`,
`roles/traefik/templates/docker-compose.yml.j2:50`) and is superficially
attractive: one attach point, no router enumeration, no drift, and it would
automatically cover every future router. **It is rejected.**

The decisive reason is not the exemption requirement (which could be met by the
§4.2 host-selection feature). It is **blast radius under daemon failure**:

- Traefik's `forwardAuth` to an unreachable address returns **500 for the
  original request**. A BitBlocker process that is stopped, crashed, OOM-killed,
  mid-`docker pull`, or wedged takes down **every router on the entrypoint** —
  including the two exempt clients who are paying specifically not to be subject
  to this, including `portfolio-service`, including umami, including any site
  that was never meant to be filtered at all.
- Under per-router attachment, that same daemon failure is confined to the
  sites that opted in. The exempt clients are **structurally** immune, not
  immune-by-config-lookup-inside-the-failing-component. That distinction is the
  whole point: an exemption that depends on the exempting process being alive
  is not an exemption.

Secondary reason: entrypoint attachment would put the geo filter in front of the
`websecure` entrypoint's every router, which is a superset of §1.6.3's "must never
carry it" list.

The cost of rejecting it is honest and stated: **per-router attachment can
drift.** A router added later without the label is silently unprotected. §1.6's
rule and the interface spec's enumeration are the mitigation; the verification
in §5.2 is the detection.

---

### 2. The intermediate state — why "the template has landed and the daemon does not exist" is harmless

This section discharges the operator's acceptance condition. It is not an
argument that the right order will be followed; it is an argument that **each of
the two mechanisms by which a router can go down is independently foreclosed**,
so the harmlessness does not rest on sequencing discipline.

#### 2.1 The two mechanisms, and why each is closed

There are exactly two ways this change can take a Traefik router out of service.

**Mechanism 1 — a router references a middleware that does not exist.** Traefik
puts such a router into an error state; it stops serving its rule. This is the
mechanism behind DevOps' stated hazard and it is the one already documented
in-repo for `asana-ip-allowlist`
(`roles/traefik/templates/dynamic.yml.j2:56-60`).

> **Closed by §1.1's invariant:** the `bitblocker` middleware is rendered
> unconditionally, in both bodies, on every host where a router might reference
> it. There is no configuration of `bitblocker_enforcing`, and no state of the
> daemon, under which the name `bitblocker` is undefined. A reference to it can
> therefore never be dangling.

**Mechanism 2 — the middleware exists and points at a dead address.**
`forwardAuth` to an unreachable target returns 500 for the original request.

> **Closed by §1.4's default:** `bitblocker_enforcing` defaults to `false`, so
> the rendered body is a `headers` middleware that talks to nothing. It cannot
> point at a dead address because it does not point anywhere. It only ever
> points at the daemon after a deliberate flip, and that flip is gated on a
> pre-flight `/healthz` 200.

Two independent guards, each of which alone would prevent the outage. The
default state of the system after this change lands is: **a header gets stamped
on some requests.** That is the entire behavioral delta.

#### 2.2 The state table

| # | State | What exists on the host | What a visitor to an `enforce` site gets | What a visitor to an `exempt` site gets | Reversal |
|---|---|---|---|---|---|
| S0 | Today | Nothing | Normal | Normal | — |
| S1 | ansible#253 merged, `--tags traefik` applied | `dynamic/bitblocker.yml` rendered, benign body. No daemon. No router labels. | Normal — an unreferenced middleware definition is inert | Normal | Delete one file |
| S2 | Site roles applied | Labels emitted on all five attached routers (§1.6) of every `enforce` site. Middleware still benign. **Still no daemon.** | Normal, plus an `X-BitBlocker-Enforcement: disabled` request header at the backend | **Normal, no label, no header** | Per-site re-apply |
| S3 | Daemon deployed, V1 passed | Daemon running and answering `/healthz` 200. Middleware still benign. | Normal | Normal | Stop the daemon — safe here, nothing points at it |
| S4 | `bitblocker_enforcing: true` | Middleware body flipped to `forwardAuth` | **Filtered** | Normal | **~seconds, one file, nothing restarted** |

**S2 is the state the operator's constraint is about, and it is a no-op.** The
routers carry a label pointing at a middleware that exists and does nothing.
This is not asserted — §5.2's V0b observes it, by checking for the stamped
header at the backend on a pilot site before anything else happens.

Note also what S2 says about the exempt clients: they never acquire a label at
any state, so no BitBlocker state — including a bug in this design — has a path
to them.

#### 2.3 The one place order still matters, and the machine check that enforces it

Mechanism 1 is closed **provided the fragment is on the host before a label
references it.** In a full `site.yml` apply this is automatic: `traefik` is in
the `roles:` block (`site.yml:185`) and every site family is deployed from the
`tasks:` block (`site.yml:221-256`), which runs after all roles. Verified
against the playbook.

The reachable violation is a **tag-scoped apply**: `--tags wordpress` against a
host whose `traefik` role has not been re-applied since this change would emit
labels with no fragment present. That is a runbook-step-away-from-an-outage,
which under the operator's constraint is not good enough.

> **Therefore: a site role must not emit `bitblocker@file` unless
> `{{ traefik_proxy_dir }}/dynamic/bitblocker.yml` is present on the host. If it
> is absent and the site is `geo_filter: enforce`, the play fails, naming the
> site and instructing the operator to include the `traefik` tag.**

**Fail, not silently omit.** Omitting would leave a site the operator believes
is protected silently unprotected — the exact decorative-control failure this
repo has a documented history of (`roles/wordpress/templates/docker-compose.yml.j2:122-134`,
`:140-153`). Failing costs nothing: the check runs before any template is
rendered, so no change has been made when it fires, and the remedy is to add one
tag to the command. Like §1.4's asserts, it carries its own tag so it can be
deliberately skipped for one apply.

This converts the ordering requirement from a procedure into a property.

#### 2.4 What this design does *not* make free

Two costs, named rather than buried:

- **Reaching S2 recreates 12 WordPress containers.** Changing a compose file's
  labels means `docker compose up` recreates that container; each site has a few
  seconds of interruption. `-e wordpress_site_filter=<site>` scopes it to one
  site at a time (`site.yml:23-27`), so this is 12 small interruptions the
  operator schedules, not one big one. It is also why attachment is done *early
  and once*, while the daemon does not exist and the middleware is benign —
  paying this cost at a moment when nothing can go wrong is strictly better than
  paying it during the flip.
- **S4 is a real change with a real blast radius.** Nothing here makes turning
  enforcement *on* safe; it makes turning it on **cheap to undo**. That is the
  achievable property, and §5.3's lever 1 is where it is cashed.

---

### 3. Fail-mode posture: `fail-closed` stays. Do not set `fail-open` on app01.

This is the decision with the most at stake, so the analysis is given before the
recommendation.

#### 3.1 What "unusable" actually costs on a shared host

One daemon serving ~16 filtered sites means the unusable-blocklist state is a
multi-tenant event. Under `fail-closed`, every attached site returns the
configured block status (403) to **every visitor, from every country**, for the
duration. That is a real, correctly-feared outcome and it deserves the
arithmetic rather than a posture preference.

#### 3.2 The realistic paths to "unusable", with their windows

`UNUSABLE ⇔ lookup == nil || lookup.Len() == 0` (interface spec
`fail-open-and-readiness.md` §2.1). Verified against the code, there are exactly
three ways to reach it, and one of them is far more likely than ADR 0004
assumed.

**Path 1 — a failed refresh on a running daemon. Cannot happen.** Every failure
path in `fetcher.Refresh` returns before `f.source.Swap(trie)`
(`internal/fetcher/fetcher.go:139-170`). `Swap` is the only mutator and is
called only after a fully successful load. A daemon whose refreshes have been
failing for a month keeps serving its last good trie. **Window: none. This path
is closed.**

**Path 2 — `ready-then-empty`: `block.countries` matches nothing in the
dataset.** A typo'd or nonexistent country code produces a valid MMDB load with
`Len() == 0`. This is a config error, is caught immediately by the
`ever_ready: true` + `likely_cause` heartbeat (ADR 0004 §D.2), and is fixed by
correcting the config. **Window: until the operator reads the ERROR log.**
Bounded by verification (§5.2), which will catch it before any site is attached.

**Path 3 — cold start with no usable cache.** This is the one that matters, and
the mechanism is worse than ADR 0004 §E.2 recorded.

#### 3.3 The cache-freshness defect — the finding that changes the analysis

ADR 0004 §E.2 asserts:

> "ADR 0002's cache (default `max_age` 48h) means a routine restart during a
> DB-IP outage serves cached data and never reaches the predicate at all. The
> uncovered window is a restart after >48h of both downtime and fetch failure."

**That bound does not hold in the steady state.** Traced through the code:

1. Staleness is measured against the cache file's **modification time**
   (`internal/diskcache/cache.go:108` — `now.Sub(info.ModTime()) > maxAge`).
2. The mtime advances **only** when `Refresh` takes the `OutcomeUpdated` branch
   and calls `diskcache.Write` (`internal/fetcher/fetcher.go:157`). A `304 Not
   Modified` returns at line 147-150, **before any cache write**.
3. The fetcher sends conditional-GET validators (`If-None-Match` /
   `If-Modified-Since`) from `f.etag` / `f.lastModified`
   (`internal/fetcher/fetcher.go:216-221`), populated on the last successful
   download.
4. DB-IP publishes **monthly** (ADR 0003; `dbip-country-lite-YYYY-MM.mmdb.gz`).
   The default refresh cadence is **daily** (`0 3 * * *`,
   `internal/config/config.go:163`).

So a healthy, long-running daemon downloads once, then gets `304` on every
subsequent daily fetch for the rest of the month. **The cache file's mtime is
pinned to the last actual download and ages past the 48h `max_age` within two
days** — after which the cache is stale for the remaining ~28 days of the month.

And on the next restart, `loadDiskCache` does not merely skip a stale cache — it
**deletes it** (`cmd/bitblocker/main.go:195-199`, `removeUnusableCache`,
OQ-CACHE-2 ratified 2026-07-19). The daemon then starts UNUSABLE with no cache
on disk to fall back to, and the deletion is not undone by restarting again.

The corrected picture of Path 3:

| Step | Outcome |
|---|---|
| Restart at any time >48h after the last actual download | Cache is stale → **deleted** → daemon starts UNUSABLE |
| Cold-start fetch succeeds (validators are empty after restart, so this is an unconditional GET → 200 → full download → cache rewritten) | Window is **seconds**. This is the overwhelmingly common case. |
| Cold-start fetch fails | 8 attempts, backoff `2·2ⁿ` capped at 5m ⇒ **~4m14s of retries** (`internal/scheduler/scheduler.go:19-23, 164-194`), then the budget is exhausted and the daemon **waits for the next cron tick — up to ~24h** |

**The uncovered window is therefore not "a restart after >48h of both downtime
and fetch failure." It is "any restart that coincides with a DB-IP fetch
failure,"** because the cache is stale-and-therefore-deleted for most of any
given month. On a host where `restart: unless-stopped` plus an ansible re-apply
plus a host reboot are all routine, that is a materially more likely event than
ADR 0004 costed.

**One conditional, stated honestly.** The whole mechanism depends on
`download.db-ip.com` actually serving `ETag` and/or `Last-Modified`. If it
serves neither, `f.etag`/`f.lastModified` stay empty, every daily fetch is
unconditional, every fetch rewrites the cache, and the mtime is never more than
24h old — the defect does not exist. **This is one command to settle and it is a
prerequisite to the fix, not to this ADR:**

```
curl -sSI https://download.db-ip.com/free/dbip-country-lite-2026-08.mmdb.gz | grep -iE '^(etag|last-modified):'
```

The §3.5 remedy is correct and harmless in both cases; the verification decides
whether it is urgent or merely tidy.

#### 3.4 What `fail-open` would trade away, and why it is the wrong instrument

`fail-open` would convert the Path-3 tail from "16 sites down" into "16 sites
unfiltered." That is a real improvement on availability and a real regression on
the thing the daemon exists for, and on a shared host the regression is worse
than it looks:

- The failure becomes **silent to visitors and to clients**. Nobody reports it.
  Detection collapses onto one channel — an ERROR log heartbeat — on a host
  whose alerting on that channel does not exist yet (§ Open questions).
- The state is **sticky**. The same restart-plus-fetch-failure that opens the
  window also leaves no cache on disk, so the daemon can sit `ever_ready: false`
  indefinitely, allowing everything, while `/healthz` 503s into a monitor
  nobody has wired.
- It is the **decorative-control** outcome. The 2026-07-21 fleet compromise on
  this same host (`roles/wordpress/templates/docker-compose.yml.j2:126-131`) is
  the record of what a security control that was quietly not working costs on
  app01.

And critically: **`fail-open` would be compensating for a defect, not for a
posture.** Flipping the posture to paper over a cache that is not doing its job
leaves the cache not doing its job, and buys the availability with the daemon's
entire purpose. ADR 0004 §E already refused the loud-to-silent trade, for
reasons that hold *more* strongly on a shared host, not less: sixteen tenants
make the silent failure harder to notice, not easier.

**ADR 0004's rejection of time-bounded fail-open also stands and is not
reopened.** Its reasoning — that a posture which is a function of elapsed time
rather than of configuration is illegible to an operator debugging live, and
that it reintroduces the 403s at the worst moment — is correct and this ADR
adopts it unchanged.

#### 3.5 The third option that *is* warranted: serve stale rather than serve nothing

There is a better lever than either posture, and it does not touch
`startup_mode` at all. Two changes, in priority order:

**Fix A (required before the app01 rollout) — make `cache.max_age` mean what
ADR 0002 says it means.** On `OutcomeUnchanged` (a 304), the daemon has just
received affirmative upstream confirmation that its cached bytes *are* the
current published artifact — evidence of freshness at least as strong as a 200.
Advance the cache file's mtime at that moment (`os.Chtimes`). A healthy daemon's
cache then never exceeds one refresh interval in age, every routine restart
finds a fresh cache, and the uncovered window collapses to exactly the bound
ADR 0004 §E.2 claimed but did not have: *a restart after >48h of continuous
fetch failure.*

This is a **defect fix**, not a feature: it repairs a guardrail that the
fail-closed posture was explicitly costed against.

**Fix B (recommended, v1.1) — a stale cache becomes a last-resort fallback
instead of being deleted.** Today `ErrStale` ⇒ skip **and delete**. Instead:
retain the stale file, and if the cold-start retry budget is exhausted with no
usable blocklist, load it and serve it with a loud, recurring ERROR naming its
age. Month-old geo data is materially better than no geo data: its failure mode
is a handful of reassigned prefixes, versus a total outage of every attached
site. This preserves the fail-closed contract exactly — the daemon still never
allows traffic it cannot evaluate — while removing the 24h tail.

Both are specified implementation-ready in
`docs/interfaces/cache-freshness-and-stale-fallback.md`. **Neither is
implemented here.** Fix A is a Developer pass that should land before the
rollout reaches Phase 3; Fix B is Sprint 5 scope.

#### 3.6 The recommendation, stated plainly

**Deploy app01 with `behavior.startup_mode: fail-closed` (the default). Do not
set `fail-open`.** Land Fix A first. Take Fix B in Sprint 5.

The shared-host multiplier is a real concern and the answer to it is **not** a
posture flip — it is the four structural bounds this ADR puts around the
failure, none of which is a config knob:

1. **Fix A** removes the common path to the failure (§3.5).
2. **Per-router attachment** (§1.7) bounds who is affected when it happens: the
   exempt clients and every unfiltered service are untouched by any BitBlocker
   failure, including a total process failure.
3. **The default-off enforcement switch** (§1.4) means the failure is only
   reachable after a deliberate flip that is itself gated on the daemon being
   healthy.
4. **The file-provider kill switch** (§5.3) turns a 16-site outage into a
   ~10-second single-file edit that restarts nothing and needs no full apply.

Availability on this host is bought with blast-radius containment and a fast
rollback, not by asking the security control to stop working when it is
confused.

---

### 4. One policy, one daemon. No second instance for app01.

Jeff's stated requirement is binary — filtered / not filtered — and per-router
attachment expresses exactly that with a single daemon and no code change.
**A second policy is not built, not staged, and not designed for.** (KISS;
coding-standards §15 YAGNI.)

#### 4.1 Growth path, tier 1: N policies = N daemon instances

If app01 later needs two distinct country lists, the answer is a second
BitBlocker container with its own config, its own port, its own cache volume,
and its own file-provider middleware (`bitblocker-strict@file`). Sites reference
whichever middleware their policy calls for. The daemon is a single small
static binary with a ~30 MB dataset; two or three instances is a non-event.

Note that the §1.1 invariant applies per middleware name: a second middleware
name means a second always-defined fragment, with its own benign body.

This scales cleanly to roughly 3–4 policies. Past that the per-instance cache
and fetch duplication, and the operator burden of keeping N configs coherent,
start to argue for tier 2.

#### 4.2 Growth path, tier 2: `X-Forwarded-Host` policy selection — **specced, and explicitly NOT in scope for app01**

This is the feature that would let one daemon carry a policy table keyed by
hostname. It is written up here so it is not re-derived later, and because its
security analysis has a **blocking prerequisite that is not currently
satisfied.**

**Shape.** A new config block, additive, with the existing `block.countries`
retained verbatim as the default policy:

```yaml
block:
  countries: [CN, RU]          # unchanged — the DEFAULT policy

policies:                       # new, optional; absent ⇒ today's behavior exactly
  default: block.countries      # implicit; not written
  hosts:
    exempt-client-a.com: none   # named policy "none" = allow all
    www.exempt-client-a.com: none
    exempt-client-b.com: none
```

`/check` reads `X-Forwarded-Host`, normalizes it (lowercase, strip port, strip
trailing dot, punycode form), looks it up, and applies the matched policy;
an unmatched or absent host falls to the default policy.

**Contract rules that are not negotiable if this is ever built:**

1. **An unmatched host applies the default policy — never "no policy."** The
   fail direction for a hostname the operator forgot to list must be *more*
   filtering, not less. Consequence, stated up front: an exempt client whose
   `www.` or alias hostname is omitted gets filtered, which is a **loud** failure
   (the client calls) rather than a silent one (a site quietly stops being
   protected). That asymmetry is the reason for the rule.
2. **The default policy stays `block.countries`,** so a config with no
   `policies:` block behaves byte-identically to today. Additive extension,
   coding-standards §14.
3. **`X-Forwarded-Host` is never consulted for the unparseable-client-IP
   branch.** ADR 0004 §A.1's carve-out is untouched: input validity is not
   data availability, and it is not policy selection either.
4. **A `/healthz` field must expose the loaded policy count**, or an operator
   cannot tell a policy table that failed to load from one that matched nothing.

**Security analysis.**

The instinctive objection is that the Host header is client-controlled. In the
normal case that objection is weaker than it looks: Traefik routed the request
by matching `Host(...)` against the very same value it then puts in
`X-Forwarded-Host`. A client claiming `Host: exempt-client-a.com` is routed to
exempt-client-a's backend and exempted — which is precisely the configured
policy for that site. The exemption is *coextensive with the routing decision*,
so a "spoof" buys the attacker nothing they could not get by simply visiting the
exempt site.

**The real problem is `trustForwardHeader`.** BitBlocker's client-IP contract
requires `trustForwardHeader: true` on the `forwardAuth` middleware
(`docs/traefik-integration.md` § Register the middleware) — and that setting is
about whether Traefik **passes through** the incoming `X-Forwarded-*` headers
rather than overwriting them with its own. If, under `trustForwardHeader: true`,
Traefik forwards a *client-supplied* `X-Forwarded-Host` in preference to the one
derived from `req.Host`, then the header is genuinely attacker-controlled and
**decoupled from the routing decision** — at which point any client from a
blocked country appends `X-Forwarded-Host: exempt-client-a.com` to a request for
a filtered site and walks straight through. That is a total bypass of the
control.

**Therefore: this feature is blocked on an empirical determination**, against the
deployed Traefik version, of whether `trustForwardHeader: true` causes a
client-supplied `X-Forwarded-Host` to reach the auth server. It is not a design
question and must not be settled by reading documentation. Until it is settled
and, if necessary, mitigated (e.g. by requiring Traefik to strip inbound
`X-Forwarded-Host` at the entrypoint), the feature does not enter a sprint.

**Not in scope for the app01 deployment, for four independent reasons:** the
requirement is binary and tier-0 already serves it; it needs a code change on a
released binary; the blocking verification above is unresolved; and — decisively
— it only pays off in combination with entrypoint-level attachment, which §1.7
rejects on blast-radius grounds regardless.

---

### 5. Rollout, blast radius, and rollback

#### 5.1 Phased rollout

The phases follow §2.2's state table. Note that the daemon is deployed **after**
the labels, not before — the labels are inert until S4, and attaching them while
nothing can go wrong is the point.

| Phase | State reached | Scope | Blast radius if wrong |
|---|---|---|---|
| **0** | S1 | ansible#253 merged; `--tags traefik` applied. Fragment rendered, benign body. No daemon, no labels. | **None.** An unreferenced middleware definition is inert. |
| **1** | S2 (pilot) | Attach to **one BitSalt-owned site**, via `-e wordpress_site_filter=<site>`. Middleware still benign. Run V0b. | One BitSalt property, one container recreate. |
| **2** | S2 (fleet) | Attach the remaining `geo_filter: enforce` WordPress sites, one filtered apply each. Middleware still benign. | Per-site container recreates. Still no filtering anywhere. |
| **3** | S3 | Deploy the daemon. Run V1. Middleware still benign. | **None** — nothing references the daemon yet. |
| **4** | S4 | `bitblocker_enforcing: true`. Run V2 + V3 against the pilot before leaving it on. | The enforcing set. Reversible in ~10s by §5.3 lever 1. |
| **5** | — | Node.js / FastAPI sites, if in scope — requires new `middlewares=` plumbing in `roles/webapp`, which does not exist today. | Those sites. |

The two exempt clients are `geo_filter: exempt` and are **never attached at any
phase**. `portfolio-service`'s `/internal/` routes and the ACME HTTP-01 path are
never attached at any phase (§1.6.3).

Phases 0–3 have no user-visible effect and are therefore easy to compress or
skip. They are where the contract gets proven; skipping them moves that
discovery into Phase 4, where filtering is live.

#### 5.2 Verification that the control is not decorative

Five checks. V0/V0b are new with this reconciliation and are the ones that
discharge the operator's constraint empirically rather than by argument. V3 is
the one most likely to be skipped and is the one that catches the failure mode
with precedent on this host.

**V0 — the definition is inert (Phase 0).** After `--tags traefik`, confirm
Traefik logs no file-provider parse error for `dynamic/bitblocker.yml`, confirm
the middleware appears in Traefik's runtime config, and confirm **all 18 sites
still serve**. This proves a defined-but-unreferenced middleware is harmless.

**V0b — attachment while disabled is inert (Phase 1).** On the pilot site,
confirm it serves 200 through Traefik on :443, and confirm the backend receives
`X-BitBlocker-Enforcement: disabled`. The header is *why* this check is possible:
without it, "the benign middleware did nothing" and "the middleware was not
applied at all" look identical. **Do not proceed to Phase 2 without this check** —
it is the empirical form of §2's whole argument.

**V1 — daemon in isolation (Phase 3, from app01 itself):**

```
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: <IP in a blocked country>' http://<daemon>:8080/check   # expect the block status
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: <US IP>'                    http://<daemon>:8080/check   # expect 200
curl -s http://<daemon>:8080/healthz                                                                                   # expect 200, "serving":"enforcing", prefixes > 0
```

Then restart the daemon and re-check `/healthz` — it must return to 200 without
a visible window, which is the observable proof that Fix A landed and the cache
is being used.

**V2 — end to end through the edge (Phase 4), using a canary country.** Testing
the real list requires an egress in a blocked country, which is usually not
available. Instead, temporarily add to `block.countries` a country you *can*
egress from (a VPN endpoint, a cheap VPS), confirm the pilot site returns the
block status through Traefik on port 443 from that egress, confirm a US egress
still returns 200, then remove the canary and re-verify. This proves the whole
chain — Traefik's header handling, the middleware attachment, the daemon's
decision — rather than the daemon alone.

**V3 — the inert-control check (Phase 4).** BitBlocker takes the **rightmost**
`X-Forwarded-For` entry. If Traefik's `forwardAuth` call presents XFF as
`<client>, <traefik-internal-IP>` rather than a bare `<client>`, the rightmost
entry is Traefik's own address, which is in no blocked country, and **the daemon
allows everything while looking perfectly healthy** — 200s on `/healthz`, a
populated trie, no errors. The control would be entirely decorative and nothing
in V1 would reveal it.

Detect it by enabling `behavior.log_blocked` (default true) and, at Phase 4,
also `behavior.log_allowed` temporarily, then confirming the redacted client
addresses in the daemon's logs **vary across requests** rather than being one
constant `172.21.x.x`. A constant internal address is the signature of this
failure. Do not leave enforcement on beyond the pilot without this check.

The header contract is believed clean on app01 — Traefik scopes
`forwardedHeaders.trustedIPs` to the docker-proxy subnet
(`roles/traefik/templates/docker-compose.yml.j2:44,46`), so an untrusted public
peer's own XFF is discarded and rewritten from the real TCP peer, and
`ansible/lessons/umami-geo-granularity-client-ip-header.md` independently
confirms nothing fronts app01. **That is a reason to expect V3 to pass, not a
reason to skip it.** A CDN placed in front of one site later would invalidate it
silently; that is a named residual risk, not a current condition.

**V4 — the kill switch, including its restart-free property (Phase 4).** Flip
`bitblocker_enforcing` to `false` and back. Confirm Traefik picks up each change
within seconds, the pilot site keeps serving throughout, and — the specific
claim under test — **`docker ps` shows the Traefik container's uptime
unchanged across both flips.** This is the check that would have caught the
defect §1.5 fixes; do not accept the lever as working without observing the
uptime.

#### 5.3 Rollback levers, in order of speed

1. **Fleet-wide, ~10 seconds, no restarts:** set `bitblocker_enforcing: false`
   and render `dynamic/bitblocker.yml` in its benign form. Traefik's file
   watcher picks it up; no container is restarted, no router label changes, no
   site role is re-applied. **This is the incident lever**, and its restart-free
   property depends entirely on §1.5's separate fragment — it is not available if
   the middleware is folded into `middlewares.yml`.
2. **Per-site, minutes:** `geo_filter: exempt` on one site, re-apply that site's
   role with `-e wordpress_site_filter=<site>`. Costs a container recreate.
3. **Full removal, one apply:** all sites to `exempt`, remove the daemon. Leave
   the benign fragment in place — removing it is not a rollback step and, if any
   label survives anywhere, removing it is an outage.

**The anti-lever, named because the instinct under incident is exactly wrong:**
**do not stop or `docker rm` the BitBlocker container to "turn it off."** A
`forwardAuth` middleware pointing at a dead address makes Traefik return **500
for every attached site** — stopping the daemon converts a partial problem into
a total outage of everything it was attached to. Lever 1 exists precisely so
that this is never the reachable option. It belongs in the runbook in these
words.

**The second anti-lever:** do not "disable" by deleting the middleware
definition or the fragment. §1.1's invariant is what makes every label safe;
deleting the definition breaks every router that names it. Lever 1 changes the
fragment's *body*, never its existence.

#### 5.4 The daemon must not have a container HEALTHCHECK

`roles/wp_container_autoheal` installs a host-side timer that runs
`docker ps --filter health=unhealthy` and restarts **any** container on app01
reporting unhealthy (`roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`).
It is fleet-scoped, not WordPress-scoped (`site.yml:174-183`).

If BitBlocker's container declares a `HEALTHCHECK` wired to `/healthz`, this
composes into a genuinely harmful loop: an unusable blocklist ⇒ `/healthz` 503
(deliberately, ADR 0004 §C) ⇒ container marked unhealthy ⇒ autoheal restarts it
⇒ the restart re-runs `loadDiskCache`, which **deletes** a stale cache and resets
the cold-start retry budget ⇒ still unusable ⇒ restart again at the cooldown
interval. Every iteration destroys recovery state and buys nothing, because
**restarting a daemon that has no blocklist does not give it a blocklist.**

`/healthz` is a **readiness** signal, not a liveness signal — ADR 0004 §C says
this in as many words. Therefore:

> **BitBlocker's container declares no `HEALTHCHECK`.** Readiness is monitored
> externally (`/healthz` polled from the host, plus alerting on the ERROR
> heartbeat via the existing Vector→Loki path). Nothing restarts the daemon on
> a `/healthz` 503.

The shipped image is distroless with no shell or fetch tool, so the naive
`HEALTHCHECK` does not work anyway (`docs/traefik-integration.md` § `/healthz`).
This constraint exists to stop a well-intentioned wrapper image from
reintroducing it.

---

## What `bitsalt/ansible#253` must change

Stated plainly, as the delta from the DevOps design to the settled contract.
This ADR does not edit the ansible repo; these are the items for a DevOps pass
on that branch.

1. **Replace `bitblocker_country_block_enabled` at the per-site level with
   `geo_filter`** — required, string enum `enforce` | `exempt`, no default
   (§1.3). Add an unconditional assert (tagged `always` so it runs on
   tag-scoped applies) over every enabled site dict — **not** only the ones the
   current `*_site_filter` selects, or a missing key on an unselected site hides
   until the next full apply. The failure message names the site, both legal
   values, and the recommended value.
2. **Add `geo_filter` to all 18 site vars files in the same change.** The key is
   required, so the change cannot land piecemeal. The two exempt clients get
   `exempt`; Node.js and FastAPI sites get `exempt` until Phase 5 plumbing
   exists (their roles emit no `middlewares=` label today, so `enforce` there
   would be a silent lie).
3. **Keep the fleet switch and its `false` default; rename it
   `bitblocker_enabled` → `bitblocker_enforcing`** (§1.4). **The default needs no
   change** — `group_vars/all/vars.yml` already ships `false` and that is
   correct. The rename is required because the variable's *meaning* changes: it
   stops being a conjunct of `item_bitblocker_enabled` gating label emission and
   becomes the selector for the middleware's body. Keeping the name would carry
   the old semantics into the new template. Add the two pre-flight asserts
   (`bitblocker_address` defined; `/healthz` returns 200) under their own
   skip-tag, following the shape of `roles/traefik/tasks/main.yml:98-107`.
4. **Render the middleware from its own fragment,
   `{{ traefik_proxy_dir }}/dynamic/bitblocker.yml`, and do not attach
   `notify: Restart traefik` to that task** (§1.5). Render it
   **unconditionally**, in one of two bodies. This needs a short amendment to
   `bitsalt/ansible` ADR 0020 recording the per-concern deviation and its reason
   — the notify on `middlewares.yml` makes an enforcement flip a fleet-wide
   Traefik recreate. See OQ-J.
5. **Emit router labels from `geo_filter` alone, not from the fleet switch**
   (§1.2). Labels are static and inert; this is what keeps enforcement flips off
   the container-recreate path.
6. **Router coverage: no change.** DevOps' five attachment points — apex
   (`:121`), `-login` (`:138`), `-www` (`:185`), `-alias-N` (`:201`),
   `-alias-N-www` (`:229`) — are adopted as-is, as is the `-block-batch` (`:157`)
   exception and the alias-inherits-parent rule (§1.6). Keep
   `bitblocker@file` first in every chain. The only edit here is that the
   labels are now driven by `geo_filter` alone (item 5), not by
   `item_bitblocker_enabled`.
7. **Add the fragment-presence guard** (§2.3): a site role must fail, not
   silently omit, if it would emit `bitblocker@file` while
   `dynamic/bitblocker.yml` is absent from the host. Own skip-tag.
8. **Daemon placement:** no published port, cache on a named volume or bind
   mount, pinned image tag, no `HEALTHCHECK`. Interface spec §2 and §7.

**Where each item stands relative to `bitsalt/ansible#253` as it is written:**
items 1 and 2 replace the per-site key; item 3 is a rename with the default left
alone; items 4, 5 and 7 are structural changes the branch does not have; item 6
is a confirmation, not a change; item 8 is placement detail. **Item 4 is the only
one that contradicts a ratified ansible ADR** (0020). Item 6 is the one place
where the DevOps branch was already right and this ADR's first draft was wrong.

---

## Consequences

### Positive

- **The intermediate state is harmless by construction, not by sequencing.**
  Both mechanisms that could drop a router are independently foreclosed (§2.1),
  and the one residual ordering requirement is enforced by a machine check
  rather than a runbook step (§2.3). This is the operator's acceptance condition,
  discharged.
- **The incident lever is genuinely restart-free.** Because the middleware has
  its own fragment with no notify, flipping enforcement changes one small file
  and Traefik's watcher picks it up. Under the original design the same flip
  would have recreated the shared edge proxy for all 18 sites — a defect found
  only by reconciling the two PRs against the live repo.
- **The expensive, disruptive step is where nothing can go wrong.** Twelve
  container recreates happen while the middleware is a header stamp and no
  daemon exists. The dangerous step is one file and ten seconds.
- **The exemption is structural.** The two exempt clients are unaffected by any
  BitBlocker state — unusable blocklist, wedged process, stopped container, bad
  country list, or a bug in this design. Their sites never carry the label at
  all. No config lookup inside a possibly-failing daemon stands between them and
  their traffic.
- **A defect in a shipped guardrail is found and named** (§3.3). `cache.max_age`
  has been effectively inoperative in the steady state since the fetcher landed,
  and ADR 0004's security analysis was costed against a bound the code does not
  provide.
- **The fail-closed posture is preserved without asking anyone to accept a 24h
  outage tail.** Fix A closes the common path; Fix B closes the tail; neither
  weakens the contract that the daemon never allows traffic it cannot evaluate.
- **The `-login` router bypass and the autoheal restart loop are both caught at
  design time.** Attaching only to the apex router would have left WordPress
  login reachable from every blocked country; wiring a `HEALTHCHECK` to
  `/healthz` would have produced a restart loop that destroys cache state.

### Negative

- **Three moving parts instead of one.** An operator must hold "is this site
  declared for filtering," "is enforcement on," and "does the middleware exist"
  as separate facts. The §1.1 table and the §2.2 state table exist to make that
  legible, but it is genuinely more to know than a single boolean.
- **A deviation from a ratified ansible ADR.** ADR 0020's split-by-kind rule
  does not survive this change intact. The deviation is narrow and reasoned, but
  it is a precedent, and the next per-concern fragment will cite it.
- **Per-router attachment can drift.** A router added later without
  `bitblocker@file` is silently unprotected, and this repo has a documented
  history of exactly that class of error on exactly these routers. Mitigated by
  §1.6's rule and the interface spec's enumeration; detected only by
  verification, which is a periodic cost rather than a structural guarantee.
  This is the price of rejecting the entrypoint lever and it is accepted
  knowingly. **Adopting DevOps' attach-to-everything-except-unconditional-
  rejecters rule (§1.6.1) reduces this cost** — it removes the per-new-router
  judgment call that the narrower "can terminate at the backend" rule required —
  but it does not eliminate it: someone must still remember to add the label.
- **A required `geo_filter` key is a breaking change to every site vars file.**
  All 18 must carry it before the next apply, and forgetting one fails the play.
  That loudness is the design intent, but it is friction, and it means the
  ansible change cannot land piecemeal.
- **The required key does not steer toward `enforce`.** It makes omission loud;
  it does not make the safe value the easy one. The assert message and the
  greppable exemption set are the only mitigations.
- **Node.js and FastAPI sites need new plumbing.** `roles/webapp` emits no
  `middlewares=` label today, so Phase 5 is genuinely new template work rather
  than adding a name to an existing list.
- **Fix A is a code change to a released binary,** which means a Developer pass,
  a QA pass, and (per the project's own Sprint 4 lesson) a rehearsed tag before
  the rollout reaches Phase 4.
- **`block.countries` remains a single global list.** If the fleet later needs
  two policies, §4.1's second instance is operationally clumsy and §4.2 is
  blocked on an unresolved security verification.

### Neutral

- **Redirect-only routers are attached, though they cannot leak content.** A
  blocked visitor to `www.<site>` or an alias gets a 403 rather than a 301 into a
  403. The security outcome is identical either way (§1.6.1); the attachment
  buys a saved round trip and a simpler rule, at the cost of three more labels
  per site and three more `/check` calls per redirect-path request.
- **An alias redirecting off-host has its redirect geo-filtered.** The
  destination is not ours and its policy is not ours, but the 301 we issue is.
  Harmless; noted because it will look odd to someone reading an alias's
  `to_url` for the first time.
- **The disabled rendering adds a request header to backends.** Inert to
  WordPress, Node, and FastAPI, and load-bearing for V0b — it is what makes
  "attached but disabled" observable rather than assumed.
- **Labels stay on the routers when enforcement is off.** A reader of a site's
  compose file sees `bitblocker@file` on a host where nothing is being filtered.
  That is the intended shape (declaration is static), but it will read as
  surprising once and should be commented in the template.

---

## Alternatives considered

### DevOps' original: a boolean per-site key with `default: true`

Simpler, matches the `enabled:` convention already in the site dicts, needs no
edit to 18 vars files to land, and — the real argument, from ansible#253 §3 — is
fail-safe by asymmetry: an omitted flag blocks the site and fails loudly, rather
than leaving it silently unprotected.

**Why not:** the asymmetry argument is right and defeats default-`false`, but it
compares only the two defaults. Default-`true`'s loud failure surfaces at
*traffic time* and is detected by a visitor or a client hitting a 403 on a live
client site; the required key's loud failure surfaces at *apply time*, before any
container is recreated, and is detected by the operator at zero cost. Same
fail-safe property, no bill. On this fleet there is a second cost: default-`true`
would drop a new client's overseas visitors from the moment of deployment,
silently answering OQ-B — which is still open — on that client's behalf. Full
comparison in §1.3. **Conceded:** the default *steers* toward protection and the
required key only *demands an answer*; that is a real advantage this design does
not have.

### Architect's original: attach only to routers that can terminate at the backend

Two labels per WordPress site instead of five. Fewer `/check` calls, a narrower
surface for a daemon failure to touch, and provably no bypass — a blocked
visitor following a `www.` 301 lands on the attached apex and is blocked one hop
later.

**Why not:** the bypass analysis holds, but the rule requires a per-router
judgment call every time a router is added, and per-router drift is already the
accepted cost of rejecting entrypoint attachment (§ Consequences) — a rule that
adds judgment calls makes that cost worse. "Redirect-only routers *may* omit it"
is a permission, and permissions decay into "this one looked redirect-only."
DevOps' rule — everything except routers that already reject unconditionally —
cannot under-attach and needs no judgment. The certificate objection that would
have blocked adoption does not hold: `-www` and alias routers use standalone
DNS-01 (`tls.certresolver=le`), not HTTP-01, so filtering them cannot break
renewal. §1.6.1.

### Architect's original: `bitblocker_enforcing` defaulting to `true`

One less thing to remember to set.

**Why not:** DevOps is right. A switch that points a security control at a daemon
which may not exist must default to not pointing at it. The default is the
system's behavior on every host that has not been explicitly configured,
including future hosts and staging; "on" is the wrong answer for all of them.

### One variable doing both jobs (fleet switch also gates label emission)

The shape both original designs implied: when the fleet switch is off, no labels
are emitted at all.

**Why not:** it puts every enforcement change on the container-recreate path.
Turning enforcement on or off would rewrite 12 compose files and recreate 12
containers on the shared host — minutes each way, with a per-site interruption,
at exactly the moment (an incident) when speed and reversibility matter most.
Separating them costs a little conceptual complexity and buys a ten-second,
restart-free, zero-recreate incident lever.

### Omit the middleware definition when enforcement is off

The intuitive reading of "disabled."

**Why not:** a Traefik router referencing a middleware that does not exist stops
serving its rule. "Disable by deleting the definition" is therefore an outage of
every attached site — the opposite of a kill switch. This repo already carries a
standing warning about exactly this shape for `asana-ip-allowlist`
(`roles/traefik/templates/dynamic.yml.j2:56-60`). §1.1's invariant exists to make
the mistake unavailable.

### Put the `bitblocker` middleware in `middlewares.yml` per ansible ADR 0020

Complies with a ratified ADR; no deviation to justify; one fewer file.

**Why not:** the task that renders `middlewares.yml` carries
`notify: Restart traefik` (`roles/traefik/tasks/main.yml:48-55`) and that handler
recreates the container. Every enforcement flip would recreate the shared edge
proxy for all 18 sites. The kill switch's whole value is that it is fast and
touches nothing else; inheriting that notify removes the value. A narrow,
documented deviation is the cheaper price.

### Achieve the toggle inside the daemon instead of in Traefik

E.g. an "allow all" mode toggled by config and a reload signal.

**Why not:** the daemon has no such mode and no reload path, so this is a code
change on a released binary to obtain something the file provider already gives
for free. It also makes the kill switch depend on the daemon being responsive —
and the state you most need the kill switch in is the state where it is not.
Setting `block.countries: []` is worse still: it produces `Len() == 0`, which is
UNUSABLE, which under fail-closed is 403 for everyone.

### Attach at the `websecure` entrypoint and exempt inside BitBlocker

One attach point, immune to router drift, automatically covers every future
router. Needs §4.2's host-selection feature to express the exemption.

**Why not:** blast radius. `forwardAuth` to an unreachable address is a 500, so
any BitBlocker process failure takes down every router on the entrypoint —
including the two exempt clients, `portfolio-service`, and umami. An exemption
implemented *inside* the component whose failure you are worried about is not an
exemption. It also puts the geo filter in front of the ACME HTTP-01 path and the
webhook routes (§1.6.3's never-attach list). This is the closest alternative to
being right and it is rejected on a single decisive property; recorded at length
because its drift-immunity is genuinely attractive and it will be re-proposed.

### Define the middleware via docker labels on the daemon's container

Consistent with how the per-site WordPress middlewares are defined.

**Why not:** it puts the kill switch behind a container recreate, and it couples
the middleware's existence to the daemon container's lifecycle — so the
enforcement toggle and the thing being toggled fail together, and stopping the
daemon would delete the middleware and break every attached router.

### Set `startup_mode: fail-open` on app01

Trade filtering for availability on a host with 16 filtered tenants.

**Why not:** §3.4. It converts a loud failure into a silent one on a host where
detection is a single unwired log channel, it makes the failure sticky (no cache
left on disk to recover from), and it compensates for a defect (§3.3) instead of
fixing it. ADR 0004 §E made this trade-off call once already; the shared-host
multiplier strengthens its reasoning rather than overturning it.

### Time-bounded fail-open (allow for N minutes after cold start, then revert)

Cap the exposure by treating fail-open as a startup grace period.

**Why not:** ADR 0004 § Alternatives rejected this and that rejection stands
unchanged. A posture that is a function of elapsed time rather than of
configuration is illegible to an operator debugging live, and it reintroduces
the 403s at the worst moment. Listed here only to record that it was
reconsidered under the shared-host framing and the answer did not change.

### Raise `cache.max_age` to 720h instead of fixing the mtime

Set the max age past a month so the never-advancing mtime stops mattering.

**Why not:** it makes `max_age` mean nothing rather than making it mean
something, and it silently accepts serving month-old geo data on a routine
restart — which is the exact outcome ADR 0002 § Alternatives ("No staleness
bound — serve any loadable cache") rejected. Fix A costs one `os.Chtimes` call
and preserves the bound's meaning; Fix B is the *deliberate*, loudly-logged
version of "serve stale," gated on the retry budget being exhausted rather than
applied unconditionally.

### Run BitBlocker as a systemd unit on the host instead of a container

The shipped `packaging/systemd/bitblocker.service` has `DynamicUser=true`,
`CacheDirectory=bitblocker`, and a full hardening set, and it sidesteps §5.4's
autoheal interaction entirely.

**Why not (for app01):** it is genuinely defensible and is DevOps's call, not
this ADR's. The container path matches how everything else on app01 is deployed
and keeps the daemon on the `proxy` network with no published port. Recorded so
DevOps can choose the systemd path deliberately if the host-side ergonomics
favor it; the only constraints this ADR places on that choice are the cache
persistence requirement, the never-publicly-reachable requirement, and §5.4.

---

## Open questions surfaced

- **OQ-A (Jeff, operator input — blocks Phase 4).** The content of
  `block.countries`. This is a business decision about which markets the hosted
  sites serve, not an architecture one, and this ADR deliberately does not pick
  it. Needed as a concrete ISO 3166-1 alpha-2 list before enforcement is turned
  on.
- **OQ-B (Jeff, product-level — blocks Phase 4, and is a stop condition).** The
  ~15 non-exempt sites belong to hosting clients too. Attaching a country filter
  drops their overseas visitors, including any legitimate ones — expatriate
  customers, overseas staff reaching wp-admin, referral traffic. **Have those
  clients been told, and is the decision BitSalt's to make on their behalf?**
  This is a client-commitment question and the architecture pass will not decide
  it. Note that OQ-3 (per-IP allowlist for admin/monitoring addresses) is still
  open and unbuilt, so there is **no mechanism today** to except a named
  overseas address from an otherwise-filtered site — the only granularity
  available is whole-site.
- **OQ-C (Jeff/DevOps — blocks Phase 1).** Which site is the Phase-1 pilot. Must
  be BitSalt-owned, not a client site. `bitsalt-staging` is the natural choice
  but is a Node.js site, which needs Phase-5 plumbing first; the pragmatic pick
  is a low-traffic BitSalt-owned WordPress property.
- **OQ-D (DevOps — blocks Fix A's priority, not the ADR).** Does
  `download.db-ip.com` return `ETag` and/or `Last-Modified`? One `curl -sSI`
  (§3.3) settles whether the cache-freshness defect is live or latent.
- **OQ-E (DevOps/Developer — blocks §4.2 only, not this deployment).** Under
  `trustForwardHeader: true` on Traefik v3.6.13, does a client-supplied
  `X-Forwarded-Host` reach the `forwardAuth` target, or does Traefik overwrite
  it from `req.Host`? Must be settled empirically, not from documentation.
- **OQ-F (DevOps).** Is there any external uptime monitor, synthetic check, or
  third-party integration hitting these sites from outside the US? Anything of
  that shape must be enumerated before Phase 4 or it will be blocked with no
  allowlist available to except it (see OQ-B).
- **OQ-G (DevOps).** Alerting on the ERROR heartbeat. The `logging` role's
  Vector→Loki path exists on app01; §3.4's argument against fail-open depends on
  the unusable-blocklist state being *noticed*. Recommend a Loki alert on
  `check: blocklist still unusable` before Phase 4.
- **OQ-H (Jeff).** Scope: are `umami` (`analytics.bitsalt.com`) and
  `portfolio-service` in the filtered set at all? Both carry machine traffic
  (HTTP-01 ACME validation, GitHub webhooks). Architect lean: **exclude both**;
  the benefit is negligible and the failure modes are silent.
- **OQ-I — RESOLVED 2026-08-03.** Tracked the gap left by writing the first
  draft without the `bitsalt/ansible#253` diff. The diff has since been read and
  supplied. Outcome: three claims corrected (the fleet switch is
  `bitblocker_enabled`, not `bitblocker_country_block_enabled`; it already ships
  `false`, so the bad default was Architect's own; DevOps attaches five routers,
  not two — adopted in §1.6). Two claims confirmed (the per-site key really is an
  optional boolean; label emission really is gated on both switches). No part of
  the settled contract was overturned. Detail in § Reconciliation.
- **OQ-J (DevOps/ansible-side Architect — blocks item 4 of the ansible delta).**
  `bitsalt/ansible` ADR 0020 mandates file-provider split by kind; §1.5 requires
  a per-concern fragment for `bitblocker`. That ADR needs a short amendment
  recording the deviation and its reason (the `notify: Restart traefik` blast
  radius), filed in the ansible repo, not here.
- **OQ-K (DevOps — verification, not a blocker).** Confirm that Traefik v3.6.13
  loads a defined-but-unreferenced file-provider middleware without warning or
  error. Expected to be a non-event; V0 is where it is observed. If Traefik does
  warn, the warning will recur on every reload and should be documented so it is
  not mistaken for a fault.

---

## Cross-references

- `docs/interfaces/traefik-middleware-attachment.md` — the DevOps-facing
  contract derived from §1, §2, §5.3, and §5.4.
- `docs/interfaces/cache-freshness-and-stale-fallback.md` — the Developer-facing
  spec for §3.5's Fix A and Fix B.
- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` §A, §C, §E —
  the posture this ADR preserves; §E.2 carries a correction block filed by this
  pass.
- `docs/adr/0002-disk-cache-snapshot-format.md` §C and § Alternatives ("No
  staleness bound") — the guardrail §3.3 finds inoperative.
- `docs/adr/0003-geoip-source-db-ip-over-maxmind.md` — the monthly publication
  cadence that drives the 304-vs-mtime interaction.
- `docs/deployment.md` § Client-IP trust, § Run mode (a) — the cache-volume and
  never-publicly-reachable requirements this ADR inherits unchanged.
- `docs/traefik-integration.md` § Request contract, § Response contract.
- `internal/fetcher/fetcher.go:139-170, 216-221` — the 304 short-circuit and the
  conditional-GET validators.
- `internal/diskcache/cache.go:99-120` — mtime-based staleness.
- `cmd/bitblocker/main.go:190-221` — `loadDiskCache` and `removeUnusableCache`.
- `internal/scheduler/scheduler.go:19-23, 164-194` — the cold-start retry budget.
- `internal/server/server.go:216-278` — `handleCheck`; the absence of any
  `X-Forwarded-Host` read.
- `bitsalt/ansible`: `site.yml:23-27` (per-site filter), `:89-141` (fleet
  composition), `:174-183` (fleet-scoped autoheal ordering), `:185` (traefik role
  position), `:221-256` (site loops run after all roles);
  `roles/traefik/tasks/main.yml:40-55` (the dynamic-config render and its
  notify), `:98-107` (the fail-loudly-with-a-skip-tag precedent);
  `roles/traefik/templates/docker-compose.yml.j2:39-40,44,46,50,65-68`;
  `roles/traefik/templates/dynamic.yml.j2:56-60` (the referenced-but-undefined
  middleware warning);
  `roles/wordpress/templates/docker-compose.yml.j2:118-121` (apex),
  `:122-138` (`-login`), `:140-158` (`-block-batch`), `:159-189` (`-www`,
  including the standalone DNS-01 comment at `:160-181`), `:190-205`
  (`-alias-N`), `:206-234` (`-alias-N-www`);
  `roles/wordpress/meta/argument_specs.yml` (`item.options`, where
  `bitblocker_country_block_enabled` is declared on `bitsalt/ansible#253`);
  `group_vars/all/vars.yml` (`bitblocker_enabled: false`, as shipped on that
  branch);
  `roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`;
  ADR 0020 (file-provider layout), ADR 0005 (per-site named dict vars).
- Coding standards §14 (additive extension over redefinition — §4.2's
  `policies:` block), §4 (explicit boundaries), §15 (YAGNI/KISS — §4).

---

*End of ADR 0005.*
