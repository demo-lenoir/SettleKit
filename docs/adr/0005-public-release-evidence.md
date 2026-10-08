# ADR 0005: Public release evidence

Status: accepted, 2026-10-05.

## Context

The Sepolia contracts and two payment scenarios predate the current application revision. A public release must prove what is observable now without implying that the current backend handled those historical transactions. Git commit ancestry alone cannot establish chain state, deployed runtime, or webhook delivery.

## Decision

The tracked `docs/sepolia-evidence.json` contains chain ID, deployed addresses, transaction hashes, exact scenario terms, source-verification URLs, and the original source repository identifier. The ignored release sidecar adds the current commit, its public CI repository, and its GitHub checks URL. The strict release checker requires a clean working tree and successful CI for that exact commit. It also rebuilds the contract artifacts and compares compiled runtime and source hashes with the live Sepolia contracts and Etherscan source. It verifies canonical successful deployment, funding, release, and refund receipts, the expected calldata and events, distinct escrow identities, and at least six confirmations.

Historical off-chain webhook results remain recorded observations. They are not reproducible from the retained material because the ephemeral signing secrets and signatures were discarded. A successful check of current receipts can be invalidated by a later chain reorganization or a dishonest RPC provider.

## Consequences

The current release gate is independent of old GitHub run URLs. It is stronger than a commit-label check for the deployed contracts because it compares actual runtime and verified source. It cannot prove that the current backend ran the historical Sepolia scenarios; local Anvil and PostgreSQL integration tests cover current backend behavior. No audit or production-readiness claim follows from these checks.
