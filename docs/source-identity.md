# Contract source identity

The Sepolia contracts and payment scenarios are historical evidence. No contract was redeployed for this standalone repository. The public transaction and address record remains in [`sepolia-evidence.json`](sepolia-evidence.json). The `github_repository` value in that record identifies the prior source repository; it is not a claim that a commit in this new Git history was deployed.

The retained public chain record does not identify a backend commit that handled the historical transactions. This snapshot has a new local Git history; its initial commit is the publication of the current source tree.

[`source-identity.sha256`](source-identity.sha256) records SHA-256 hashes for all 44 Solidity files used by the `PaymentEscrow` and `MockUSDC` compiler artifacts, plus `foundry.toml`. The listed digests describe the Solidity inputs in this snapshot. Verify the local copy with `shasum -a 256 -c docs/source-identity.sha256`.

Foundry 1.8.4 with Solidity `0.8.37+commit.f401782d` produced the same `PaymentEscrow` and `MockUSDC` creation bytecode, runtime bytecode, compiler metadata, and per-source Keccak hashes as the source repository's existing build artifacts. The key compiled source hashes are `0x8e8a44e0bbab4fe1d799559a6c44ec89274d6af518094dd9fe5335d428675916` for `PaymentEscrow.sol` and `0x108da6b22939c150723c1806041a396f35ba25045c44b62889f95d71ea96444d` for `PaymentEscrow.t.sol`, which contains `MockUSDC`.

These local comparisons establish the identity of this copy's compiler inputs with the retained compiler inputs. The strict `make verify` separately compares compiled runtime with the live Sepolia code and source hashes with Etherscan's verified source. It also checks canonical transaction receipts and exact-commit CI for the repository selected in the local release sidecar. Those live checks are required before treating the current commit as a verified release; they do not prove that the current backend handled the historical Sepolia payments.
