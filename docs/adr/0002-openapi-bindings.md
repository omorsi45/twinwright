# ADR 0002: OpenAPI plus explicit behavior bindings

Status: Accepted, 2026-09-24

Compile a constrained OpenAPI subset into operation definitions. Require a separate explicit binding for every operation. OpenAPI describes transport shapes but cannot infer refund eligibility or state changes. Reject unsupported or unbound operations at build time. The first manifest routes only known fictional billing behaviors; this prevents a misleading claim of arbitrary API simulation.
