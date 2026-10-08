#!/usr/bin/env python3

from pathlib import Path

root = Path(__file__).resolve().parents[1]
required = [
    "SPEC.md",
    "docs/threat-model.md",
    "docs/evidence.md",
    "docs/architecture.md",
    "docs/runbook.md",
    "docs/demo.md",
    "api/openapi.yaml",
    "Makefile",
    ".github/workflows/verify.yml",
    "docs/adr/0001-escrow-protocol.md",
    "docs/adr/0002-identity-and-chain-consistency.md",
    "docs/adr/0003-operator-transactions-and-http-auth.md",
    "docs/adr/0004-idempotency-and-webhooks.md",
]
missing = [name for name in required if not (root / name).is_file()]
if missing:
    raise SystemExit("Required documentation missing: " + ", ".join(missing))

spec = (root / "SPEC.md").read_text()
for term in (
    "NONE -> FUNDED -> RELEASED | REFUNDED",
    "claimExpiredRefund",
    "PAUSER_ROLE",
    "Idempotency-Key",
    "X-SettleKit-Signature",
    "Maximum automatic reorg depth",
):
    if term not in spec:
        raise SystemExit(f"SPEC missing decision: {term}")

evidence = (root / "docs/evidence.md").read_text()
if evidence.count("\n|") < 25:
    raise SystemExit("Evidence matrix is incomplete")

openapi = (root / "api/openapi.yaml").read_text()
for route in ("/v1/payment-intents:", "/v1/payment-intents/{id}/release:", "/metrics:"):
    if route not in openapi:
        raise SystemExit(f"OpenAPI missing route: {route}")

print("Documentation and interface checks: PASS")
