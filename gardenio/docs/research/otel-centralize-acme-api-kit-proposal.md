# Proposal: Centralize OTel init in acme-api-kit

**Status:** Proposal — operator review pending
**Bead:** ka-0000006707
**Related:** ka-0000001300 (per-app ECS attr porting), ka-0000005134 (parent epic)
**Estimate:** 2–3 engineering days

## Problem statement

Every Python service in Platform that emits OpenTelemetry traces ships its own
near-verbatim copy of `app/config/tracing.py`. Confirmed duplicates today:

- `sponsor-api`
- `site-api`

Likely additional copies (assumed from naming + the parent epic surface area):

- `sponsor-connect`
- `site-connect`
- `sysadmin-api`
- `discovery-api`
- `tsda-api`

Each copy contains the same building blocks:

1. `setup_tracing(app, settings)` — wires `TracerProvider`, OTLP exporter,
   FastAPI / SQLAlchemy / requests instrumentation.
2. A custom `TracingSpanProcessor` that adds baggage / request-scoped
   attributes to every span.
3. A `HealthCheckFilterSampler` that drops spans for `/health`, `/ready`, etc.
4. DB caller-info enrichment (filename / function / line of the SQL caller).

The cost of this duplication is paid every time we touch tracing:

- The current ECS-attr enrichment (`cloud.account.id`, `aws.ecs.cluster.arn`,
  `aws.ecs.task.arn`) requires **N near-identical MRs**, one per service.
  See ka-0000001300 for the per-app porting bead and the trail of children
  (sponsor-api, site-api, study-web, …).
- Each MR is its own review, its own deploy, its own chance to drift.
- The next OTel change (whatever it is) will pay the same tax.
- Subtle divergence has already crept in: span-processor ordering and the
  exact set of baggage keys differ slightly between sponsor-api and site-api.

The ratio of work to value here is bad and getting worse as we add services.

## Proposed shape

Move the shared pieces into **acme-api-kit** (`example-org/acme-api-kit`)
under a new `apikit.observability` module:

```text
acme-api-kit/
└── apikit/
    └── observability/
        ├── __init__.py
        ├── tracing.py            # setup_tracing, exporter, instrumentation
        ├── span_processor.py     # TracingSpanProcessor (baggage → span attrs)
        ├── sampler.py            # HealthCheckFilterSampler
        └── db_caller_info.py     # SQLAlchemy caller-info enrichment
```

### Public API

```python
# apikit/observability/tracing.py

from typing import Iterable, Mapping, Optional
from fastapi import FastAPI

class TracingSettings:
    """Subset of app settings that observability cares about.

    Apps can pass their existing Settings object as long as it exposes these
    attributes; we duck-type rather than couple to a concrete pydantic model.
    """
    otel_enabled: bool
    otel_service_name: str
    otel_exporter_endpoint: Optional[str]   # OTLP HTTP/gRPC endpoint
    environment: str                         # e.g. "prod", "rwx", "dev"

def setup_tracing(
    app: FastAPI,
    settings: TracingSettings,
    *,
    baggage_keys: Iterable[str] = (),
    resource_attributes: Optional[Mapping[str, str]] = None,
    extra_instrumentations: Iterable[Instrumentor] = (),
) -> None:
    """Idempotent OTel init. Safe to call once at app startup.

    Args:
        app: FastAPI app to instrument.
        settings: object exposing the TracingSettings attributes above.
        baggage_keys: per-app baggage keys to copy onto every span
            (e.g. ("sponsor.slug",) for sponsor-api). Empty tuple is fine.
        resource_attributes: extra Resource attrs merged on top of the
            kit-provided defaults (service.name, deployment.environment,
            cloud.account.id, aws.ecs.cluster.arn, aws.ecs.task.arn, ...).
        extra_instrumentations: app-specific Instrumentor instances the kit
            doesn't know about (rare).

    No-op when settings.otel_enabled is false.
    """
```

### What the kit owns

- `TracerProvider` construction + OTLP exporter wiring.
- Default `Resource` attributes:
  - `service.name`, `service.version`, `deployment.environment`
  - ECS attrs (`cloud.account.id`, `aws.ecs.cluster.arn`, `aws.ecs.task.arn`)
    detected from the ECS task metadata endpoint when available.
- `TracingSpanProcessor` that copies the configured baggage keys onto
  every span as attributes.
- `HealthCheckFilterSampler` (configurable path list, sane default).
- DB caller-info enrichment for SQLAlchemy.
- FastAPI / SQLAlchemy / requests / httpx instrumentation registration.

### What the apps still own

- Env wiring: `OTEL_ENABLED`, `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_ENDPOINT`,
  etc. — pulled into the app's existing `Settings` object.
- App-specific baggage keys passed in via `baggage_keys=`.
  - sponsor-api: `("sponsor.slug",)`
  - site-api: probably `()` (TBD — see open questions)
  - sponsor-connect / site-connect: TBD per-app
- Calling `setup_tracing(app, settings)` once during app startup.

This shrinks each app's tracing.py from ~150 lines to ~5.

## Migration plan

### Phase 1 — Ship apikit release (~1 day)

1. Add `apikit/observability/` to acme-api-kit on a feature branch.
2. Port the canonical implementation from sponsor-api (most recently updated
   per ka-0000001300). Reconcile the small drifts vs site-api.
3. Add unit tests:
   - `setup_tracing` is idempotent.
   - Disabled-mode is a true no-op (no exporter, no provider override).
   - `HealthCheckFilterSampler` drops `/health` and configurable paths.
   - `TracingSpanProcessor` copies baggage onto spans.
   - ECS resource detection degrades gracefully off-ECS.
4. Cut a acme-api-kit minor release (e.g. `0.x.0` → `0.(x+1).0`) and publish.

### Phase 2 — Per-app bumps (~1.5 days, parallelizable)

For each app (sponsor-api, site-api, sponsor-connect, site-connect,
sysadmin-api, discovery-api, tsda-api, …):

1. Bump acme-api-kit to the new minor.
2. Replace `app/config/tracing.py` body with:
   ```python
   from apikit.observability import setup_tracing as _setup_tracing
   def setup_tracing(app, settings):
       _setup_tracing(app, settings, baggage_keys=("sponsor.slug",))
   ```
   …or delete the file and call `apikit.observability.setup_tracing` directly
   from the FastAPI app factory.
3. Verify trace export still works in the app's RWX/dev env (smoke test:
   one request → one trace in Grafana with the expected attrs).
4. MR per app, individually reviewed, individually deployed.

### Phase 3 — Cleanup (~0.5 day)

1. Remove the now-dead per-app tracing modules where fully replaced.
2. Update Platform service-template / cookiecutter (if one exists) to call
   `apikit.observability.setup_tracing` out of the box.
3. Document in acme-api-kit README how new services should wire tracing.

### Total: 2–3 engineering days

Phase 1 is sequential. Phases 2 and 3 are parallelizable across apps and
can be split across the team or driven by gardenio crew.

## Open questions for operator review

1. **acme-api-kit version constraint.** Should `setup_tracing` go into the
   next minor (`0.(x+1).0`) and require all apps to bump together, or do we
   want a feature-flag path that lets old apps stay on the prior minor while
   new apps adopt? Recommendation: minor bump + per-app bumps; we already
   own all the consumers and the migration is small.

2. **Module naming.** `apikit.observability` vs `apikit.tracing` vs
   `apikit.otel`. `observability` is most future-proof (logs, metrics) but
   right now we're only doing tracing. Recommendation: `observability`.

3. **Settings duck-typing vs explicit dataclass.** Above proposes
   duck-typing on the app's `Settings`. Alternative: define
   `apikit.observability.TracingConfig` as a real pydantic model and have
   each app construct one from its `Settings`. The dataclass version is
   stricter but adds a tiny bit of boilerplate per app.

4. **Baggage key registry.** Today sponsor-api has `sponsor.slug`; other
   apps will have their own. Should the kit ship a small registry of
   well-known keys, or stay agnostic and let each app pass whatever it
   wants? Recommendation: stay agnostic; document the convention in
   the kit README.

5. **Scope of "observability."** This proposal is tracing-only. We have
   a parallel question about whether to also centralize structured logging
   and Prometheus metrics in `apikit.observability`. Recommendation: defer;
   land tracing first, evaluate after.

6. **Coverage of non-FastAPI services.** `setup_tracing(app, settings)` is
   FastAPI-shaped. Do we have services that need tracing but aren't
   FastAPI (workers, Celery, scripts)? If yes, we may want a sibling
   `setup_tracing_worker(settings, baggage_keys=…)` entry point. Worth
   confirming before the kit API lands.

7. **Rollout ordering.** Bump the smallest-blast-radius app first
   (probably tsda-api or discovery-api) to shake out the kit API before
   touching sponsor-api / site-api, which carry the most traffic.
   Recommendation: yes.

## Why we should do this now

- The next OTel enrichment is already on the backlog (per ka-0000001300 /
  ka-0000005134). Doing it the old way costs another N MRs.
- The drift between sponsor-api and site-api tracing.py is small today; it
  will be larger after the next round of porting.
- 2–3 days of investment buys us "OTel changes are one MR, not N" forever.

## Out of scope for this bead

- Implementation. This bead is proposal-only.
- Logs and metrics centralization (see open question #5).
- Non-Python services (study-web/JS has its own tracing story tracked
  separately).
