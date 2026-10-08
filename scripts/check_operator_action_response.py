#!/usr/bin/env python3

import argparse
import json
import pathlib
import re
import sys


def validate(status, body, chain_id, contract, calldata):
    if status != 202:
        raise ValueError(f"operator action returned HTTP {status}, expected 202")
    if not isinstance(body, dict) or set(body) != {"chain_id", "to", "data", "value"}:
        raise ValueError("operator action body has unexpected fields")
    if type(body["chain_id"]) is not int or not all(isinstance(body[key], str) for key in ("to", "data", "value")):
        raise ValueError("operator action body has unexpected field types")
    if body["chain_id"] != chain_id or body["value"] != "0":
        raise ValueError("operator action chain or ETH value differs")
    if body["to"].lower() != contract.lower() or body["data"].lower() != calldata.lower():
        raise ValueError("operator action destination or calldata differs")


def self_test():
    body = {"chain_id": 11155111, "to": "0x" + "a" * 40, "data": "0x" + "b" * 72, "value": "0"}
    validate(202, body, 11155111, body["to"], body["data"])
    try:
        validate(200, body, 11155111, body["to"], body["data"])
    except ValueError:
        pass
    else:
        raise AssertionError("HTTP 200 was accepted")
    print("operator action HTTP 202 evidence check: PASS")


def main():
    if sys.argv[1:] == ["--self-test"]:
        self_test()
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("http_status", type=int)
    parser.add_argument("response_file", type=pathlib.Path)
    parser.add_argument("chain_id", type=int)
    parser.add_argument("contract")
    parser.add_argument("expected_calldata")
    args = parser.parse_args()
    if not re.fullmatch(r"0x[0-9a-fA-F]{40}", args.contract):
        parser.error("expected contract address must be 20 bytes")
    if not re.fullmatch(r"0x[0-9a-fA-F]{72}", args.expected_calldata):
        parser.error("expected operator calldata must contain selector and bytes32 escrow ID")
    raw = args.response_file.read_bytes()
    if len(raw) > 4096:
        raise SystemExit("operator action response exceeds 4 KiB")
    try:
        validate(args.http_status, json.loads(raw), args.chain_id, args.contract, args.expected_calldata)
    except (ValueError, TypeError, KeyError) as exc:
        raise SystemExit(str(exc)) from None
    print("operator action HTTP 202, chain, destination, value and calldata: PASS")


if __name__ == "__main__":
    main()
