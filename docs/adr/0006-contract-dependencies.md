# ADR 0006: Contract dependency snapshot

Status: accepted, 2026-10-05.

The contract and Foundry tests use a fixed subset of OpenZeppelin Contracts and forge-std. A standalone checkout includes the exact source files required for compilation at their existing import paths, plus their upstream licenses. The source revisions are recorded beside the vendored files. No upstream source file is edited.

This keeps the contract compiler inputs stable and lets the live release checker compare the resulting source hashes and runtime bytecode with the verified Sepolia contracts. Dependency updates require a source review, a new vendor snapshot, Foundry and Slither checks, and a fresh comparison with deployed bytecode. A changed compiler input must not be described as the already deployed contract.
