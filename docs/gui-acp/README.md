# Crush Desktop ACP Implementation Specification

Status: Draft for implementation.  
Target: Crush desktop and GUI clients.  
Compatibility baseline: Agent Client Protocol (ACP) as implemented by
`internal/acp`.

This directory specifies a high-performance, feature-complete backend contract
for desktop clients. Standard ACP remains the interoperability surface. Desktop
features that ACP does not express are exposed as negotiated `crush/*` JSON-RPC
extensions. Implementations MUST preserve standard ACP behavior for clients that
do not negotiate these extensions.

The primary architectural rule is that live presentation state MUST NOT depend
on SQLite flush cadence. Provider events feed an in-memory session event stream
for the GUI and a separate persistence writer for durable history.

## Normative language

The terms **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are to
be interpreted as requirements. A work package is complete only when its linked
requirements and acceptance criteria are satisfied.

## Document map

- [Product requirements](00-product-requirements.md)
- [Architecture](01-architecture.md)
- [Protocol specification](02-protocol-spec.md)
- [Versioned protocol field schemas](schema/v1/)
- [Server implementation](03-server-implementation.md)
- [Client state model](04-client-state-model.md)
- [Performance and reliability](05-performance-and-reliability.md)
- [Security](06-security.md)
- [Testing, delivery, and agent work packages](07-delivery-and-work-packages.md)
- [Autonomous goal/loop execution runbook](08-autonomous-execution.md)
- [Goal/loop copy-and-run quick start](09-goal-loop-quickstart.md)
- [Implementation status checkpoint](IMPLEMENTATION-STATUS.md)

This specification builds on [ACP hardening](../acp-hardening-plan.md). ACP
conformance bugs in that plan remain valid work; this specification does not
redefine standard ACP.

## Compatibility and negotiation

During `initialize`, the server SHOULD advertise an experimental capability:

```json
{
  "experimental": {
    "crush": {
      "protocolVersion": 1,
      "features": [
        "sessionSync",
        "sessionControl",
        "terminal",
        "blob",
        "blobUpload",
        "clientFS",
        "providerAuth",
        "mcpControl"
      ]
    }
  }
}
```

The client MUST echo the selected protocol version and features. The server
MUST reject unsupported `crush/*` requests with JSON-RPC method-not-found or a
defined `CRUSH_FEATURE_NOT_NEGOTIATED` error. Adding an optional field is
backward compatible. Removing or changing the meaning of a field requires a new
major `protocolVersion`.

## Implementation invariants

1. `internal/acp` owns standard ACP compatibility; it MUST NOT become the
   desktop application's domain-service layer.
2. `internal/guiapi` owns `crush/*` methods and calls shared application
   services. Standard ACP and GUI handlers MUST NOT call each other.
3. Every session mutation is idempotent, session-authorized, and observable as
   a sequenced event.
4. Snapshot plus event replay is the recovery contract. Full-history replay is
   not a GUI synchronization mechanism.
5. Reliable events are never silently discarded.
6. MCP visibility and invocation continue to use the session capability scope,
   generation, revision, and tombstone model already implemented.
