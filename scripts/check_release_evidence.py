#!/usr/bin/env python3
import functools
import json
import os
import pathlib
import re
import subprocess
import urllib.parse
import urllib.request
import uuid

CHAIN_ID = 11155111
MIN_CONFIRMATIONS = 6
MAX_RESPONSE = 4 * 1024 * 1024


class EvidenceError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def command(*args):
    try:
        return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=120).strip()
    except (subprocess.SubprocessError, OSError):
        raise EvidenceError('local verification command failed: ' + args[0]) from None


@functools.lru_cache(maxsize=64)
def keccak(value):
    return command('cast', 'keccak', value)


def calldata(signature, *args):
    return command('cast', 'calldata', signature, *map(str, args)).lower()


def address(value):
    require(isinstance(value, str) and re.fullmatch(r'0x[0-9a-fA-F]{40}', value), 'invalid address')
    require(int(value, 16) != 0, 'zero address')
    return value.lower()


def txhash(value):
    require(isinstance(value, str) and re.fullmatch(r'0x[0-9a-fA-F]{64}', value), 'invalid transaction/hash identity')
    return value.lower()


def word(value):
    return format(int(value, 16) if isinstance(value, str) and value.startswith('0x') else int(value), '064x')


def workflow_path_valid(path):
    return bool(re.fullmatch(r'(?:\.?/)?\.github/workflows/verify\.yml(?:@[^\s]+)?', path))


def read_json(request, label):
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            raw = response.read(MAX_RESPONSE + 1)
        require(len(raw) <= MAX_RESPONSE, label + ' response too large')
        return json.loads(raw)
    except EvidenceError:
        raise
    except Exception as exc:
        raise EvidenceError(label + ' failed (' + type(exc).__name__ + ')') from None


def successful_ci_run(repository, commit):
    query = urllib.parse.urlencode({'head_sha': commit, 'status': 'success', 'per_page': 100})
    headers = {'Accept': 'application/vnd.github+json', 'User-Agent': 'SettleKit-release-check/2'}
    if os.environ.get('GITHUB_TOKEN'):
        headers['Authorization'] = 'Bearer ' + os.environ['GITHUB_TOKEN']
    data = read_json(urllib.request.Request(
        f'https://api.github.com/repos/{repository}/actions/workflows/verify.yml/runs?{query}', headers=headers), 'GitHub CI')
    return any(r.get('head_sha') == commit and r.get('status') == 'completed'
               and r.get('conclusion') == 'success' and workflow_path_valid(r.get('path', ''))
               and r.get('html_url', '').startswith(f'https://github.com/{repository}/actions/runs/')
               for r in data.get('workflow_runs', []))


def rpc(method, params):
    require(method in {'eth_chainId', 'eth_blockNumber', 'eth_getBlockByNumber',
                       'eth_getTransactionReceipt', 'eth_getTransactionByHash', 'eth_getCode'}, 'non-read-only RPC method')
    endpoint = os.environ.get('SETTLEKIT_TESTNET_RPC_URL', '')
    require(endpoint.startswith('https://'), 'SETTLEKIT_TESTNET_RPC_URL must be HTTPS')
    request = urllib.request.Request(endpoint, json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': method, 'params': params}).encode(), {'Content-Type': 'application/json'})
    data = read_json(request, 'RPC ' + method)
    require(data.get('id') == 1 and not data.get('error') and data.get('result') is not None, 'invalid RPC response for ' + method)
    return data['result']


def verified_source(contract):
    key = os.environ.get('ETHERSCAN_API_KEY', '')
    require(bool(key), 'ETHERSCAN_API_KEY required for live verified-source validation')
    query = urllib.parse.urlencode({'chainid': CHAIN_ID, 'module': 'contract', 'action': 'getsourcecode', 'address': contract, 'apikey': key})
    data = read_json('https://api.etherscan.io/v2/api?' + query, 'verified source')
    require(data.get('status') == '1' and len(data.get('result', [])) == 1, 'explorer source unavailable')
    return data['result'][0]


def check_source(record, artifact):
    require(bool(artifact['source_hashes']), 'compiled source hashes required')
    require(record.get('ContractName') == artifact['name'], 'verified contract name mismatch')
    require(record.get('CompilerVersion', '').removeprefix('v') == artifact['compiler'], 'verified compiler mismatch')
    raw = record.get('SourceCode', '')
    require(bool(raw), 'source not verified')
    try:
        source = json.loads(raw[1:-1] if raw.startswith('{{') else raw)['sources']
        require(all(isinstance(source.get(path, {}).get('content'), str) and keccak(source[path]['content']) == digest
                    for path, digest in artifact['source_hashes'].items()), 'verified source content mismatch')
    except (ValueError, KeyError, TypeError):
        raise EvidenceError('invalid verified-source document') from None


def verify_evidence(data, head, call_rpc, ci, source, artifacts, historical):
    require(set(historical) == set(data) - {"commit", "ci_repository", "ci_checks_url"}, "historical evidence fields differ")
    require(all(data[k] == v for k, v in historical.items()), "sidecar differs from reviewed historical evidence")
    require(data.get('schema_version') == 3, 'release evidence schema_version must be 3')
    require(data.get('commit') == head, 'release evidence commit does not match HEAD')
    require(data.get('chain_id') == CHAIN_ID and int(call_rpc('eth_chainId', []), 16) == CHAIN_ID, 'Ethereum Sepolia chain required')
    confirmations = data.get('confirmations')
    require(type(confirmations) is int and MIN_CONFIRMATIONS <= confirmations <= 128, 'release confirmation policy must be 6..128')
    repository = data.get('ci_repository', '')
    require(bool(re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository)), 'invalid GitHub repository')
    require(data.get('ci_checks_url') == f'https://github.com/{repository}/commit/{head}/checks', 'CI URL must bind exact HEAD')
    scenarios = data.get('scenarios', {})
    require(set(scenarios) == {'release', 'refund'}, 'separate release and refund scenarios required')
    require(ci(repository, head), 'successful exact-commit CI required for ' + head)
    token, escrow, operator = (address(data[k]) for k in ('token_address', 'escrow_address', 'operator_address'))
    require(token != escrow, 'token and escrow addresses must differ')
    for key, contract, name in [('verified_source_url', escrow, 'PaymentEscrow'), ('token_verified_source_url', token, 'MockUSDC')]:
        require(data.get(key, '').lower() == f'https://sepolia.etherscan.io/address/{contract}#code', 'verified-source URL must bind deployed address')
        check_source(source(contract), artifacts[name])
        original = data['escrow_address' if name == 'PaymentEscrow' else 'token_address']
        require(artifacts[name]['runtime'] != '0x' and call_rpc('eth_getCode', [original, 'latest']).lower() == artifacts[name]['runtime'].lower(), 'deployed runtime differs from compiled source')
    tip = int(call_rpc('eth_blockNumber', []), 16)
    cache = {}

    def receipt(h):
        h = txhash(h)
        if h in cache:
            return cache[h]
        r = call_rpc('eth_getTransactionReceipt', [h])
        require(isinstance(r, dict) and r.get('status') == '0x1' and r.get('transactionHash', '').lower() == h, 'successful matching receipt required')
        number = int(r['blockNumber'], 16)
        require(tip - number + 1 >= confirmations, 'insufficient canonical confirmations')
        block = call_rpc('eth_getBlockByNumber', [r['blockNumber'], False])
        require(block and block.get('hash') == r.get('blockHash') and int(block['number'], 16) == number, 'receipt is not canonical')
        tx = call_rpc('eth_getTransactionByHash', [h])
        require(tx and tx.get('hash', '').lower() == h and tx.get('blockHash') == r['blockHash']
                and tx.get('blockNumber') == r['blockNumber'] and int(tx['chainId'], 16) == CHAIN_ID, 'transaction chain/block mismatch')
        require(tx.get('from', '').lower() == r.get('from', '').lower() and tx.get('to') == r.get('to'), 'receipt transaction origin mismatch')
        cache[h] = r, tx
        return r, tx

    for key, contract in [('token_deployment_tx', token), ('deployment_tx', escrow)]:
        r, tx = receipt(data[key])
        require(tx['to'] is None and address(r['contractAddress']) == contract, 'deployment does not create expected contract')

    def event(r, name, signature, eid, participant=None, payload=None):
        relevant = [log for log in r['logs'] if log.get('address', '').lower() == escrow and (log.get('topics') or [None])[0] == keccak(signature)]
        require(len(relevant) == 1, 'exactly one ' + name + ' required')
        log = relevant[0]
        participants = participant if isinstance(participant, tuple) else ((participant,) if participant else ())
        topics = [keccak(signature), eid] + ['0x' + word(p) for p in participants]
        require(log['topics'] == topics, name + ' escrow/participant/topics mismatch')
        require(log.get('removed') is False and log.get('blockHash') == r['blockHash']
                and log.get('blockNumber') == r['blockNumber'] and log.get('transactionHash') == r['transactionHash'], name + ' log origin mismatch')
        if payload is not None:
            require(log.get('data', '').lower() == payload, name + ' amount/terms mismatch')
        return log

    ids, transactions = set(), set()
    for kind, scenario in scenarios.items():
        try:
            intent = uuid.UUID(scenario['intent_id'])
        except (ValueError, AttributeError):
            raise EvidenceError('invalid intent UUID') from None
        require(str(intent) == scenario['intent_id'], 'intent UUID must be canonical')
        payer, payee = address(scenario['payer']), address(scenario['payee'])
        require(payer != payee and payee != escrow, 'invalid escrow participants')
        raw_amount = scenario['amount_base_units']
        require(isinstance(raw_amount, str) and bool(re.fullmatch('[1-9][0-9]{0,38}', raw_amount)), 'integer base-unit amount required')
        amount, expiry = int(raw_amount), scenario['expires_at_unix']
        require(amount < 2**128 and type(expiry) is int and 0 < expiry < 2**64, 'invalid amount/expiry range')
        eid = txhash(scenario['escrow_id'])
        encoded = command('cast', 'abi-encode', 'f(string,uint256,address,bytes16,address,address,uint128,uint64)',
                          'SETTLEKIT_ESCROW_V1', str(CHAIN_ID), escrow, '0x' + intent.hex, payer, payee, str(amount), str(expiry))
        require(keccak(encoded) == eid and eid not in ids, 'escrow ID does not bind unique intent and terms')
        ids.add(eid)
        fund_hash, terminal_hash = txhash(scenario['fund_tx']), txhash(scenario['terminal_tx'])
        require(fund_hash != terminal_hash and not {fund_hash, terminal_hash} & transactions, 'scenarios require distinct funding/terminal transactions')
        transactions.update((fund_hash, terminal_hash))
        require(data[kind + '_tx'].lower() == terminal_hash, 'terminal field differs from scenario')
        if kind == 'release': require(data['create_tx'].lower() == fund_hash, 'create_tx differs from release funding')
        fund, fund_tx = receipt(fund_hash)
        terminal, terminal_tx = receipt(terminal_hash)
        require(address(fund_tx['to']) == escrow and address(fund_tx['from']) == payer and int(fund_tx['value'], 16) == 0, 'funding transaction parties/value mismatch')
        require(fund_tx['input'].lower() == calldata('createAndFund(bytes16,address,uint128,uint64)', '0x' + intent.hex, payee, amount, expiry), 'funding calldata differs from intent')
        event(fund, 'EscrowCreated', 'EscrowCreated(bytes32,address,address,address,uint256,uint64)', eid,
              (payer, payee), '0x' + word(token) + word(amount) + word(expiry))
        event(fund, 'EscrowFunded', 'EscrowFunded(bytes32,uint256)', eid, payload='0x' + word(amount))
        name = 'EscrowReleased' if kind == 'release' else 'EscrowRefunded'
        event(terminal, name, name + '(bytes32,address,uint256)', eid, payee if kind == 'release' else payer, '0x' + word(amount))
        require(address(terminal_tx['to']) == escrow and address(terminal_tx['from']) == operator and int(terminal_tx['value'], 16) == 0, 'terminal transaction parties/value mismatch')
        require(terminal_tx['input'].lower() == calldata(kind + '(bytes32)', eid), 'terminal calldata differs from escrow')
        require(int(terminal['blockNumber'], 16) >= int(fund['blockNumber'], 16) + confirmations, 'terminal submitted before required funding confirmations')
    final_tip = int(call_rpc('eth_blockNumber', []), 16)
    for r, _ in cache.values():
        require(final_tip - int(r['blockNumber'], 16) + 1 >= confirmations, 'confirmations changed during verification')
        b = call_rpc('eth_getBlockByNumber', [r['blockNumber'], False])
        require(b and b.get('hash') == r['blockHash'], 'canonical receipt changed during verification')


def build_artifacts(data):
    require(not command('git', 'status', '--porcelain', '--untracked-files=normal'), 'release working tree must be clean')
    command('forge', 'build', '--force', '--no-dynamic-test-linking', '--silent')
    result = {}
    for name, path in [('PaymentEscrow', 'PaymentEscrow.sol'), ('MockUSDC', 'PaymentEscrow.t.sol')]:
        artifact = json.loads(pathlib.Path(f'contracts/out/{path}/{name}.json').read_text())
        runtime = bytearray.fromhex(artifact['deployedBytecode']['object'].removeprefix('0x'))
        for refs in artifact['deployedBytecode'].get('immutableReferences', {}).values():
            require(name == 'PaymentEscrow', 'unexpected token immutable')
            for ref in refs:
                require(ref['length'] == 32, 'unexpected immutable width')
                runtime[ref['start']:ref['start'] + 32] = bytes.fromhex(word(data['token_address']))
        source_hashes = {p: item['keccak256'] for p, item in artifact['metadata']['sources'].items()}
        result[name] = {'name': name, 'compiler': artifact['metadata']['compiler']['version'], 'source_hashes': source_hashes, 'runtime': '0x' + runtime.hex()}
    return result


def main():
    path = pathlib.Path('docs/release-evidence.json')
    require(path.is_file(), 'release gate pending: docs/release-evidence.json required')
    data = json.loads(path.read_text())
    verify_evidence(data, command('git', 'rev-parse', 'HEAD'), rpc, successful_ci_run, verified_source, build_artifacts(data),
                    json.loads(pathlib.Path("docs/sepolia-evidence.json").read_text()))
    print('release evidence: exact-HEAD CI, Sepolia canonical deployment/funding/terminal receipts, escrow linkage, compiled runtime and verified sources: PASS')


if __name__ == '__main__':
    try:
        main()
    except (EvidenceError, KeyError, TypeError, ValueError) as exc:
        raise SystemExit(str(exc) if isinstance(exc, EvidenceError) else 'invalid release evidence structure: ' + type(exc).__name__) from None
