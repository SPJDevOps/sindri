# 24. Dev bootstrap token must stay loopback-only

- **Status:** Accepted
- **Date:** 2026-10-18
- **Relates to:** [ADR 18](0018-authentication-and-rbac-in-v1.md)

## Context

The development bootstrap token is intentionally simple and highly privileged. It is useful for local testing in the Compose stack, but it is not a production credential and it should never become a public admin password by accident.

A previous default change moved the bind address to loopback, which is a good safety measure, but a server started with no config and the default token can still become reachable on a public interface if an operator overrides the listen address or uses a container startup that sets `:8080` without realizing the implications.

## Decision

The default server bind is loopback-only, and the built-in development bootstrap token is only valid when the listener is loopback-bound. Startup fails with a clear error if the operator combines the default token with a public interface.

This keeps local development easy while ensuring a default token never silently grants admin access to a machine on the same network. Operators who expose a service publicly must choose a real token, or configure an identity-provider-based login flow.

## Consequences

- Local development still works with the Compose stack and the default token on `127.0.0.1:8080`.
- A public listener with `sindri-dev-token` fails fast and clearly at startup.
- The default bind is a safety default, not a promise that a public deployment is safe without operator intent.
