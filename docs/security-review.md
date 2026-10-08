# SettleKit security review

Status: implementation review and remediation, 2026-10-03. This is not an independent security audit. Anvil/fuzz/invariant tests and Slither are evidence, not a security guarantee.

## Slither 0.11.6 result

Command: `python3 scripts/check_slither.py` (invokes Slither with Foundry compilation and excludes dependency paths). Result: three `timestamp` findings, each `Low` impact / `Medium` confidence, in `createAndFund`, `release`, and `claimExpiredRefund`. No high or medium impact finding was reported. The script fails on any new finding or on a timestamp finding outside these three functions.

Each accepted finding is checked by exact detector, impact, confidence, and function in `scripts/check_slither.py`. A changed count or function set fails the gate.

| Exact Slither finding | Why accepted in this design | Affected invariant | Supporting test/evidence |
|---|---|---|---|
| `timestamp`, Low/Medium: `createAndFund`, line 101, `expiresAt <= block.timestamp || uint256(expiresAt) > block.timestamp + MAX_TTL` | A block producer can shift a near-boundary creation across the validity window, causing the transaction to revert. The transaction cannot create an underfunded or invalid-expiry escrow. A payer should leave expiry margin. | Every funded escrow has a future deadline within 30 days at execution; failed creation leaves no escrow or token debit. | `testDuplicateAndInvalidInputsRevert`, `testFuzz_AmountAndDeadline`, `testFeeTokenAndRevertingTokenLeaveNoEscrow`; `make contract-verify`. |
| `timestamp`, Low/Medium: `release`, line 123, `block.timestamp >= escrow.expiresAt` | A boundary release may revert due to the block timestamp; a successful release can only occur strictly before the recorded deadline and always pays the recorded payee. After the deadline, the payer can claim refund. | Terminal exclusivity, designated recipient, release only before expiry. | `testExpiryBoundaryAndPermissionlessRefund`, `testCreateAndReleaseExactlyOnce`, `testInterleavedTerminalCallsPreserveTwoEscrows`; `make contract-verify`. |
| `timestamp`, Low/Medium: `claimExpiredRefund`, line 140, `block.timestamp < escrow.expiresAt` | A boundary refund may revert in a block whose timestamp is still before the deadline; it cannot pay an arbitrary caller and becomes callable at or after the on-chain deadline even when paused. | Funds return only to the recorded payer; expired escrow remains refundable during pause. | `testExpiryBoundaryAndPermissionlessRefund`, `testExpiredRefundWorksWhilePausedAndOperatorMayRefundAfterExpiry`, `testEventSequenceCanReconstructExpiryRefund`; `make contract-verify`. |

The timestamp source is intrinsic to the on-chain deadline. Replacing it with off-chain wall time would add an unverifiable authority. The retained risk is execution uncertainty near the boundary, not loss of conservation or recipient control.

## Manual review focus

- One fixed token; exact balance delta on funding and payout. A fee-on-transfer token reverts creation. Token blacklist or changed token behavior can still block payout.
- Escrow ID commits to chain, contract, UUID, payer, payee, amount and expiry. A stranger using the same UUID with different terms gets a different ID; the backend must compare all event fields and ignore variant escrows.
- Stored terminal status is set before ERC-20 external call and protected by `nonReentrant`. Failure reverts status and transfer together.
- A token granted operator authority in an adversarial test cannot reenter another funded escrow during payout (`testPayoutCannotReenterAnotherFundedEscrow`). Interleaved release/refund calls across two escrows leave exactly one payout per escrow (`testInterleavedTerminalCallsPreserveTwoEscrows`).
- Release destination is the recorded payee; both refund paths go to the recorded payer. No arbitrary rescue or upgrade path.
- Admin, operator, and pauser are separate initial addresses. Admin transfer uses a two-day delay and acceptance step. Pauser cannot unpause or move funds.
- `EscrowExpired` is paired with `EscrowRefunded` only for the permissionless expiry path. An operator refund after expiry emits `EscrowRefunded` alone; watchers should derive the outcome from the refund event.

Sepolia MockUSDC source/runtime and escrow source/runtime are verified; MockUSDC is freely mintable and testnet-only. Regression verification was completed after remediation. A professional security audit and production role custody/operations plan remain absent; this project is a testnet reference implementation. CI status is checked for the exact release commit in GitHub Actions.

## Off-chain adversarial review

| Boundary and failure | Control and local evidence | Residual risk |
|---|---|---|
| Merchant/operator authorization and replay | Distinct bearer tokens of at least 32 bytes; operator routes require operator token; raw request hash and scoped idempotency result are committed with the effect. API tests cover weak configuration, wrong caller, conflicting key, concurrent opposite actions, expiry-window replay, and replay through a new API instance. | One configured merchant and operator principal are suitable only for a controlled demo. Token theft grants that principal's authority until rotation. |
| Unknown wallet submission outcome | The service returns unsigned calldata and does not hold a nonce/key or submit transactions. A wallet must read nonce, receipt, and escrow state before retry. Contract terminal exclusivity makes duplicate terminal effects revert. | A wallet may still waste gas or race another authorized operation; no hosted transaction manager is provided. |
| Spoofed or inconsistent RPC | Funding terms are matched to the stored chain/contract/token/payer/payee/amount/expiry. Canonical block parent/hash and logs share a DB transaction. Fallback must match chain ID and exact checkpoint hash. Fake-source fork/deep-fork tests and Anvil proxy outage/restart tests pass. | Matching a checkpoint does not prove provider honesty about future blocks. A compromised provider or deep fork requires an external trusted source and manual reconciliation. |
| Crash around DB commit or webhook delivery | Checkpoint, status, history and outbox commit together; worker leases and retries preserve event ID and body. Anvil tests gracefully stop/restart the watcher after observation and during receiver outage. PostgreSQL tests inject reorg transaction failure and test claim/send/ack interruption, final-attempt recovery, concurrent lease ownership, dead letter and same-ID replay. | Delivery is at least once; receiver must verify timestamp/signature and dedupe event IDs. Every possible machine-instruction crash point is not fault injected. |
| Sensitive data in telemetry and network targets | RPC transport errors omit credential-bearing URL, DB startup errors are sanitized, route labels are fixed, and API logging tests check bearer secrets stay out of logs. Nonlocal webhook targets require HTTPS and redirects are not followed. HTTP server, DB pool and workers are bounded. | URL DNS can still resolve to internal IPs for a configured HTTPS target; only trusted operators should set webhook URLs. Credential rotation needs a coordinated receiver cutover. |
| Supply chain and release | Base image is pinned by digest, final image runs as UID/GID 65532, Trivy source/image and govulncheck pass, and the SPDX SBOM and SLSA provenance are tied to the scanned image config digest. | Scans are point-in-time and cannot replace source review or runtime controls. Exact-commit CI and the separate live release gate must pass. Source verification is complete for the historical Sepolia contracts. Tooling transitive dependencies/build infrastructure are not fully immutable; temporary local SBOM/provenance are not signed public release attestations. Production role custody and a professional audit remain outstanding. |

## Findings and remediation scope

- Returning orphan block PK collision: preserve and re-canonicalize identical block/log records transactionally; reject changed identities or log sets.
- Abandoned final webhook attempt: recover expired attempt-ten leases into dead letter without sending, preserving manual same-ID replay.
- Release checker accepted unrelated evidence: anchor historical scenarios in tracked JSON; validate exact funding/terminal calldata and events, participants/amount, canonical receipts and policy, exact commit CI, runtime and explorer source hashes.
- Reorg bound counted unrelated intents: select state-changing orphan/confirmation/expiry cases before applying the bound; retain atomic refusal for 1,001 affected payments.
- Missing nonlocal confirmation policy: reject at startup before I/O.
- Stale/overstated documentation: distinguish watcher block/log checks from release receipt checks, actual tests from operational procedures, and historical backend commits from remediation HEAD.

No Solidity change or Sepolia transaction is required for these fixes. Current local tests do not retroactively validate every historical off-chain execution; historical HMAC outcomes cannot be independently recomputed from the retained redacted records.
