#!/usr/bin/env bash
_settlekit_root="$(pwd)"
if [[ ! -f "$_settlekit_root/scripts/sepolia_keystores.sh" || ! -f "$_settlekit_root/foundry.toml" ]]; then
  echo "run from the SettleKit repository root" >&2
  return 1 2>/dev/null || exit 1
fi
SETTLEKIT_KEYSTORE_DIR="$_settlekit_root/.env.settlekit-wallets"
export SETTLEKIT_KEYSTORE_DIR
export DEPLOYER_KEYSTORE="$SETTLEKIT_KEYSTORE_DIR/deployer-admin"
export OPERATOR_KEYSTORE="$SETTLEKIT_KEYSTORE_DIR/operator"
export PAUSER_KEYSTORE="$SETTLEKIT_KEYSTORE_DIR/pauser"
export PAYER_KEYSTORE="$SETTLEKIT_KEYSTORE_DIR/payer"
export PAYEE_KEYSTORE="$SETTLEKIT_KEYSTORE_DIR/payee"

if ! python3 - "$SETTLEKIT_KEYSTORE_DIR" <<'PY'
import json
import pathlib
import stat
import subprocess
import sys

directory = pathlib.Path(sys.argv[1])
if not directory.is_dir() or stat.S_IMODE(directory.stat().st_mode) != 0o700:
    raise SystemExit("Sepolia keystore directory is missing or not mode 0700")
for role in ("deployer-admin", "operator", "pauser", "payer", "payee"):
    path = directory / role
    if not path.is_file() or stat.S_IMODE(path.stat().st_mode) != 0o600:
        raise SystemExit(f"{role} encrypted keystore is missing or not mode 0600")
    document = json.loads(path.read_text())
    crypto = document.get("crypto", document.get("Crypto", {}))
    if not crypto.get("ciphertext") or crypto.get("kdf") != "scrypt":
        raise SystemExit(f"{role} is not an encrypted Foundry keystore")
    if subprocess.run(
        ["git", "check-ignore", "-q", str(path)], cwd=directory.parent
    ).returncode:
        raise SystemExit(f"{role} keystore is not ignored by Git")
    if subprocess.run(
        ["security", "find-generic-password", "-s", "SettleKit Sepolia keystore", "-a", role],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode:
        raise SystemExit(f"{role} password is absent from macOS Keychain")
PY
then
  unset _settlekit_root
  return 1 2>/dev/null || exit 1
fi
unset _settlekit_root
