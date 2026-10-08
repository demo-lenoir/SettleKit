# SettleKit

SettleKit tracks ERC-20 payments held in an escrow contract. A merchant creates a payment intent, a payer funds it from a wallet, and the service reports what the chain has actually confirmed. An operator can request the calldata for a full release or refund. The service never holds a wallet key or submits a payment transaction.

The project is a testnet reference implementation. The deployed contracts use a freely mintable mock token on Ethereum Sepolia. They have not had a professional security audit and must not receive real assets.

## Run it locally

Install Go 1.27.1, PostgreSQL 18 server tools, Foundry 1.8.4, Python 3, `jq`, `curl`, and `openssl`. From a checkout, run:

```sh
make anvil-test
```

The demo starts disposable PostgreSQL and Anvil instances. It creates and funds intents, waits for confirmations, releases and refunds separate escrows, verifies signed webhooks, restarts the service, reconciles a reorganization, and checks RPC failover. It uses local disposable accounts and sends no Sepolia transaction. [The demo guide](docs/demo.md) lists the assertions.

For focused checks, use `go test ./...`, `make db-test`, or `make contract-verify`. `make local-verify` runs the full local gate, including race and fuzz smoke tests, Slither, the Anvil scenario, a clean-clone run, vulnerability scans, an SBOM, and image provenance. `make verify` adds live Sepolia, Etherscan, and exact-commit GitHub CI checks; see [release verification](docs/release-plan.md).

## How a payment moves

1. The merchant calls `POST /v1/payment-intents` with an `Idempotency-Key`. PostgreSQL stores the intent and the response in one transaction. Retrying the same request returns the same response.
2. The payer signs the returned `createAndFund` call with a wallet. The contract records and receives the exact token amount atomically. A failed or undercredited transfer leaves no escrow.
3. The indexer reads canonical blocks and contract logs, checks every funding term against the intent, and advances the status after the configured confirmation count. Its block checkpoint, state history, and outbox entry commit together.
4. The dispatcher sends an HMAC-signed webhook. Delivery is at least once: receivers must verify the signature and timestamp and deduplicate event IDs.
5. An authenticated operator may obtain unsigned release or refund calldata. The wallet signs and submits it. Only a canonical contract event changes the recorded money state.

The contract permits one terminal payout: full release to the recorded payee before expiry, or full refund to the recorded payer. After expiry, anyone can trigger the refund. Pausing blocks new funding and release, while refunds stay available. See [the protocol specification](SPEC.md) and [escrow ADR](docs/adr/0001-escrow-protocol.md).

## Failure boundaries

| Failure | Behavior |
|---|---|
| Duplicate HTTP request | A scoped idempotency record returns the original response or rejects a conflicting body. |
| Service restart | The indexer resumes from a durable checkpoint; pending webhook events remain in the outbox. |
| Chain reorganization | Orphaned observations are reversed and the new canonical branch is replayed. A previously reported confirmation can be revoked. |
| Deep or inconsistent fork | Indexing stops and readiness fails rather than guessing a payment state. Automatic reconciliation is bounded to 64 blocks and 1,000 affected intents. |
| Receiver outage | Bounded retries use the same event ID and body. Exhausted events enter a dead letter queue for deliberate replay. |
| RPC failover | The fallback must agree on chain ID and the stored checkpoint hash. Agreement does not establish provider honesty. |

The API, indexer, and dispatcher live in [`internal/`](internal/); entry points are in [`cmd/`](cmd/). PostgreSQL constraints and forward-only migrations are in [`migrations/`](migrations/). The escrow and Foundry tests are in [`contracts/`](contracts/). The [architecture notes](docs/architecture.md), [OpenAPI contract](api/openapi.yaml), [threat model](docs/threat-model.md), and [runbook](docs/runbook.md) cover the interfaces and operating procedures.

## Sepolia evidence

The [mock token](https://sepolia.etherscan.io/address/0xA119A6483116208EC8E8823c35566AeD782bE2bA#code) and [escrow](https://sepolia.etherscan.io/address/0x9c8C9e0507f837c7F38b24EDD90B49c4842F4a17#code) have verified source on Sepolia. Separate historical intents reached release and refund. Their transaction hashes and terms are in [`docs/sepolia-evidence.json`](docs/sepolia-evidence.json), with receipts and roles in [deployment evidence](docs/deployment-evidence.md). [Source identity](docs/source-identity.md) records how this standalone tree relates to the deployed contracts. The release checker verifies canonical receipts, events, calldata, participants, compiled runtime, verified source, and CI for the current commit when configured for its published repository. These historical transactions do not establish that the current backend revision ran on Sepolia; current backend behavior is covered by local integration tests.

Six confirmations were used for the Sepolia scenarios. No finite confirmation count makes a transaction irreversible. Historical webhook logs recorded successful HMAC validation, but the ephemeral secret and signatures were not retained for independent recomputation. The system has no production custody plan, independent audit, or production operating history. [Security review](docs/security-review.md) records accepted findings and residual risks.
