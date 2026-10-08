#!/usr/bin/env python3

import json
import subprocess
import tempfile
from pathlib import Path

root = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix="settlekit-slither-") as directory:
    report = Path(directory) / "report.json"
    command = [
        "slither",
        "contracts/src/PaymentEscrow.sol",
        "--compile-force-framework",
        "foundry",
        "--exclude-dependencies",
        "--filter-paths",
        "contracts/lib",
        "--json",
        str(report),
    ]
    result = subprocess.run(command, cwd=root, text=True, capture_output=True, check=False)
    if not report.is_file():
        raise SystemExit(f"Slither produced no JSON report (exit {result.returncode}):\n{result.stderr}")
    data = json.loads(report.read_text())
    if not data.get("success"):
        raise SystemExit(f"Slither failed: {data.get('error') or result.stderr}")

    reviewed_functions = {"createAndFund", "release", "claimExpiredRefund"}
    findings = data.get("results", {}).get("detectors", [])
    unexpected = []
    observed_functions = []
    for finding in findings:
        functions = {
            element.get("name")
            for element in finding.get("elements", [])
            if element.get("type") == "function"
        }
        if not (
            finding.get("check") == "timestamp"
            and finding.get("impact") == "Low"
            and finding.get("confidence") == "Medium"
            and len(functions) == 1
            and functions <= reviewed_functions
        ):
            unexpected.append((finding.get("check"), finding.get("impact"), sorted(functions)))
        else:
            observed_functions.extend(functions)
    if unexpected or len(findings) != 3 or set(observed_functions) != reviewed_functions or len(observed_functions) != 3:
        raise SystemExit(f"Slither findings changed; review required: unexpected={unexpected}, functions={observed_functions}")
    if result.returncode not in (0, 255):
        raise SystemExit(f"Slither exited unexpectedly: {result.returncode}\n{result.stderr}")
    print(f"Slither: {len(findings)} reviewed low-impact expiry timestamp finding(s); no other findings")
