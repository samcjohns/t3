# Terminal-Trader-II Roadmap

## Goal

Build and ship a live-hosted virtual stock market system with a Frequent Batch Auction (FBA) matching core, a public API, and a web app. The engine is the source of truth and all adapters (HTTP/WebSocket/UI) are layered around it using ports-and-adapters architecture.

## Design Choices

- Use a Frequent Batch Auction (FBA) matching engine.
- Use a dynamic batch size that increases if processes time approach the heartbeat interval.
- The market service should handle no authentication, but should just accept trusted, formatted requests from the authentication service which sits in front of it.
- An API gateway (authentication service) will sit in front of the market service to handle login, user account tokens, and rate limiting. The market service will not handle any of these concerns.
- Design the entire system to be universal regardless of the service sending requests such as the web app, CLI, or third-party integrations.
- Separare portfolio/ledger service

## Guiding Constraints

- Preserve immutable payload contracts defined in handoff documentation for `Order` and `TickResult` JSON.
- Keep domain engine pure and decoupled from delivery adapters.
- Use a 10-second heartbeat to clear batches.
- Ensure concurrency safety and deterministic matching behavior.

## Development Order (Phased)

## Phase 0: Foundations and Architecture Alignment

Purpose: Freeze interfaces and delivery approach before implementation grows.

Subtasks (in order):

1. Confirm and lock domain contracts.
1. Copy the canonical `Order`, `Execution`, and `TickResult` payloads into a dedicated API contract section in docs.
1. Add explicit rule: no breaking changes to contract fields without version bump.
1. Define module boundaries.
1. Domain: `pkg/engine` (pure logic only).
1. API adapter: HTTP/WebSocket server layer.
1. Web app: separate client consuming public API.
1. Define environment and deployment targets.
1. Local development profile.
1. Staging profile.
1. Production profile.
1. Add baseline operational docs.
1. Runbook for startup/shutdown.
1. Incident triage outline.

Exit criteria:

- Contracts, boundaries, and environments are documented and agreed.

## Phase 1: Core Engine Infrastructure (Domain First)

Purpose: Implement stable, testable exchange core before building public endpoints.

Subtasks (in order):

1. Implement strong domain types in `types.go`.
1. Direction enum (`BUY`, `SELL`).
1. Order type enum (`LIMIT`, `MARKET`).
1. Structs with exact JSON tags.
1. `BatchMatcher` interface.
1. Implement FBA matcher in `fba_matcher.go`.
1. Partition buy/sell sides.
1. Sort buy descending and sell ascending.
1. Compute single clearing price for overlapping batch.
1. Produce executions at one uniform price only.
1. Return zero volume for non-overlapping books.
1. Implement orchestrator in `orchestrator.go`.
1. Thread-safe order buffer and flush.
1. 10-second ticker heartbeat.
1. Batch handoff to matcher.
1. Tick callback for downstream broadcasting.
1. Add deterministic unit tests in `fba_matcher_test.go`.
1. Limit/limit match path.
1. Market versus passive limit path.
1. No-match path.
1. Add concurrency and stability checks.
1. Race-safety validation (`go test -race`).
1. Repeatability checks for same input batch.

Exit criteria:

- Engine is deterministic, race-safe, and fully test-covered for core matching cases.
- No web framework or external dependencies in domain package.

## Phase 2: Application Infrastructure Around Engine

Purpose: Build operational scaffolding that hosts the core engine safely.

Subtasks (in order):

1. Build server composition root (`cmd/server/main.go`).
1. Instantiate matcher and orchestrator.
1. Wire heartbeat lifecycle.
1. Wire graceful shutdown (SIGINT/SIGTERM).
1. Add configuration system.
1. Environment variables for port, heartbeat interval, log level.
1. Validation and defaults.
1. Add structured logging.
1. Tick start/end logs.
1. Order submission logs (without sensitive leakage).
1. Error path logs.
1. Add lightweight persistence strategy decision.
1. Define whether state is in-memory only (MVP) or persisted.
1. If persisted, add adapter interface first, then implementation.
1. Add observability hooks.
1. Health endpoint readiness/liveness contract.
1. Basic metrics counters (orders received, ticks processed, fills).

Exit criteria:

- Engine runs reliably as a process with config, logs, and lifecycle management.

## Phase 3: Public API (HTTP + WebSocket)

Purpose: Expose safe, stable interfaces to external clients.

Subtasks (in order):

1. API contract-first design.
1. Define endpoint specs and example payloads.
1. Map transport DTOs to domain models.
1. Define error model and status codes.
1. Implement HTTP order intake endpoint.
1. `POST /orders` validation pipeline.
1. Reject malformed or semantically invalid orders.
1. Forward valid orders to orchestrator.
1. Return accepted response with traceable metadata.
1. Implement market data stream endpoint.
1. WebSocket subscribe path.
1. Broadcast `TickResult` from orchestrator callback.
1. Connection lifecycle handling (connect/disconnect/backpressure).
1. Add API safety concerns.
1. Request throttling/rate limits.
1. CORS policy for web client origin(s).
1. Optional auth scaffolding if required later.
1. Add integration tests.
1. HTTP validation and status code tests.
1. WebSocket broadcast behavior tests.
1. End-to-end tick propagation test (submit orders -> receive tick).

Exit criteria:

- External clients can submit orders and receive real-time tick results reliably.

## Phase 4: Web App (Public-Facing Client)

Purpose: Deliver a functional trading interface powered by the public API.

Subtasks (in order):

1. Define UI information architecture.
1. Order entry panel.
1. Real-time ticker and last clearing price.
1. Executions feed.
1. Order book or depth snapshot view.
1. Build API client layer.
1. REST client for order submission.
1. WebSocket client for tick subscription.
1. Reconnect strategy with exponential backoff.
1. Implement state management.
1. Single source of truth for latest tick and execution history.
1. Explicit loading, error, and disconnected states.
1. Build order submission UX.
1. Client-side validation mirroring API constraints.
1. Optimistic/pending submission state.
1. Clear success/failure feedback.
1. Add responsive layout and accessibility.
1. Keyboard navigation for form controls.
1. Color contrast and status indicators.
1. Mobile and desktop layout checks.
1. Add front-end tests.
1. Critical flow tests (submit buy/sell, stream updates).
1. Input validation and error rendering tests.

Exit criteria:

- Users can place orders and observe real-time market clears on desktop and mobile.

## Phase 5: End-to-End Hardening and Release

Purpose: Verify reliability under realistic usage and ship safely.

Subtasks (in order):

1. End-to-end scenario validation.
1. Simulate multi-user mixed order flow over multiple ticks.
1. Verify clearing logic consistency across API and UI.
1. Performance and resilience checks.
1. Burst submission handling.
1. WebSocket fan-out stability.
1. Graceful recovery from restarts.
1. Security and abuse controls.
1. Input fuzzing for API payloads.
1. Basic anti-spam/rate-limit verification.
1. Deployment pipeline and release prep.
1. Build artifact generation.
1. Staging smoke tests.
1. Production rollout checklist and rollback plan.
1. Post-release observability.
1. Alert thresholds for heartbeat failures, zero-throughput anomalies, and error spikes.

Exit criteria:

- System is production-ready with tested rollback and monitoring.

## Ordered Dependency Map

1. Contracts and boundaries must be frozen before API or UI implementation.
1. Domain engine must be implemented and tested before public endpoints are exposed.
1. Public API must be stable before web app integration begins.
1. End-to-end and performance hardening must occur after API and UI integration.

## Milestone Summary

1. M1: Engine core complete and race-safe.
1. M2: Server runtime infrastructure complete.
1. M3: Public API complete with integration tests.
1. M4: Web app complete with real-time market visibility.
1. M5: Hardening complete and production release ready.