import copy
import json
import pathlib
import unittest

import check_release_evidence as gate


class ReleaseEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads(pathlib.Path(__file__).with_name('testdata').joinpath('release-evidence.json').read_text())
        self.data = copy.deepcopy(self.fixture['evidence'])
        self.responses = copy.deepcopy(self.fixture['rpc'])

    def rpc(self, method, params):
        return copy.deepcopy(self.responses[json.dumps([method, params], separators=(',', ':'))])

    def verify(self):
        gate.verify_evidence(self.data, self.fixture['head'], self.rpc,
                             lambda repo, commit: True,
                             lambda address: copy.deepcopy(self.fixture['sources'][address.lower()]),
                             self.fixture['artifacts'], self.fixture['historical'])

    def rejected(self):
        with self.assertRaises(gate.EvidenceError):
            self.verify()

    def test_valid_separate_scenarios(self):
        self.verify()

    def test_funding_cannot_satisfy_release(self):
        self.data['release_tx'] = self.data['scenarios']['release']['terminal_tx'] = self.data['create_tx']
        self.rejected()
        self.fixture['historical'] = {k: v for k, v in self.data.items() if k not in ('commit', 'ci_repository', 'ci_checks_url')}
        self.rejected()

    def test_funding_cannot_satisfy_refund(self):
        self.data['refund_tx'] = self.data['scenarios']['refund']['terminal_tx'] = self.data['scenarios']['refund']['fund_tx']
        self.rejected()
        self.fixture['historical'] = {k: v for k, v in self.data.items() if k not in ('commit', 'ci_repository', 'ci_checks_url')}
        self.rejected()

    def test_other_escrow_cannot_satisfy_terminal(self):
        for kind in ('release', 'refund'):
            with self.subTest(kind=kind):
                original = copy.deepcopy(self.responses)
                h = self.data['scenarios'][kind]['terminal_tx']
                r = self.responses[json.dumps(['eth_getTransactionReceipt', [h]], separators=(',', ':'))]
                r['logs'][-1]['topics'][1] = '0x' + 'ab' * 32
                self.rejected()
                self.responses = original

    def test_unrelated_source_url(self):
        self.data['verified_source_url'] = 'https://sepolia.etherscan.io/address/0x' + '00' * 19 + '01#code'
        self.rejected()
        self.fixture['historical'] = {k: v for k, v in self.data.items() if k not in ('commit', 'ci_repository', 'ci_checks_url')}
        self.rejected()

    def test_wrong_chain(self):
        self.data['chain_id'] = 1
        self.rejected()

    def test_wrong_contract_address(self):
        self.data['escrow_address'] = '0x' + 'ab' * 20
        self.rejected()

    def test_wrong_event_participant_amount_or_origin(self):
        for mutation in ('topic', 'recipient', 'amount', 'contract', 'block', 'removed'):
            with self.subTest(mutation=mutation):
                original = copy.deepcopy(self.responses)
                h = self.data['release_tx']
                log = self.responses[json.dumps(['eth_getTransactionReceipt', [h]], separators=(',', ':'))]['logs'][-1]
                if mutation == 'topic': log['topics'][0] = '0x' + '00' * 32
                if mutation == 'recipient': log['topics'][2] = '0x' + '00' * 32
                if mutation == 'amount': log['data'] = '0x' + '00' * 32
                if mutation == 'contract': log['address'] = self.data['token_address']
                if mutation == 'block': log['blockHash'] = '0x' + '00' * 32
                if mutation == 'removed': log['removed'] = True
                self.rejected()
                self.responses = original

    def test_reverted_or_orphaned_or_unconfirmed_receipt(self):
        for mutation in ('reverted', 'orphaned', 'unconfirmed'):
            with self.subTest(mutation=mutation):
                original = copy.deepcopy(self.responses)
                r = self.responses[json.dumps(['eth_getTransactionReceipt', [self.data['refund_tx']]], separators=(',', ':'))]
                if mutation == 'reverted': r['status'] = '0x0'
                if mutation == 'orphaned': r['blockHash'] = '0x' + '00' * 32
                if mutation == 'unconfirmed': self.responses['["eth_blockNumber",[]]'] = r['blockNumber']
                self.rejected()
                self.responses = original

    def test_policy_cannot_be_downgraded(self):
        self.data['confirmations'] = 2
        self.rejected()

    def test_wrong_head_or_unsuccessful_ci(self):
        self.data['commit'] = '0' * 40
        self.rejected()
        self.data = copy.deepcopy(self.fixture['evidence'])
        with self.assertRaises(gate.EvidenceError):
            gate.verify_evidence(self.data, self.fixture['head'], self.rpc,
                                 lambda repo, commit: False, lambda address: {}, self.fixture['artifacts'], self.fixture['historical'])

    def test_ci_repository_is_current_release_target(self):
        self.data['ci_repository'] = 'example/FinalSettleKit'
        self.data['ci_checks_url'] = f"https://github.com/example/FinalSettleKit/commit/{self.fixture['head']}/checks"
        seen = []
        gate.verify_evidence(self.data, self.fixture['head'], self.rpc,
                             lambda repo, commit: seen.append((repo, commit)) or True,
                             lambda address: copy.deepcopy(self.fixture['sources'][address.lower()]),
                             self.fixture['artifacts'], self.fixture['historical'])
        self.assertEqual(seen, [('example/FinalSettleKit', self.fixture['head'])])
        self.data['ci_checks_url'] = self.fixture['evidence']['ci_checks_url']
        self.rejected()

    def test_unverified_or_changed_explorer_source(self):
        address = self.data['escrow_address'].lower()
        for value in ('', '{{"sources":{"wrong.sol":{"content":"wrong"}}}}'):
            self.fixture['sources'][address]['SourceCode'] = value
            self.rejected()

    def test_wrong_runtime(self):
        key = json.dumps(['eth_getCode', [self.data['escrow_address'], 'latest']], separators=(',', ':'))
        self.responses[key] = '0x00'
        self.rejected()

    def test_other_funding_receipt_cannot_be_terminal(self):
        self.data['release_tx'] = self.data['scenarios']['release']['terminal_tx'] = self.data['scenarios']['refund']['fund_tx']
        self.fixture['historical'] = {k: v for k, v in self.data.items() if k not in ('commit', 'ci_repository', 'ci_checks_url')}
        self.rejected()

    def test_rpc_wrong_chain(self):
        self.responses['["eth_chainId",[]]'] = '0x1'
        self.rejected()

    def test_funding_calldata_and_terms_must_match(self):
        for mutation in ('calldata', 'payer', 'token', 'amount', 'expiry'):
            with self.subTest(mutation=mutation):
                original = copy.deepcopy(self.responses)
                h = self.data['create_tx']
                tx = self.responses[json.dumps(['eth_getTransactionByHash', [h]], separators=(',', ':'))]
                r = self.responses[json.dumps(['eth_getTransactionReceipt', [h]], separators=(',', ':'))]
                if mutation == 'calldata': tx['input'] = '0x00000000'
                if mutation == 'payer':
                    tx['from'] = r['from'] = self.data['operator_address'].lower()
                if mutation in ('token', 'amount', 'expiry'):
                    created = next(l for l in r['logs'] if len(l['topics']) == 4)
                    words = [created['data'][i:i+64] for i in range(2, len(created['data']), 64)]
                    words[{'token': 0, 'amount': 1, 'expiry': 2}[mutation]] = '0' * 64
                    created['data'] = '0x' + ''.join(words)
                self.rejected()
                self.responses = original

    def test_canonical_change_during_verification(self):
        calls = {}
        def changing_rpc(method, params):
            result = self.rpc(method, params)
            if method == 'eth_getBlockByNumber':
                n = params[0]
                calls[n] = calls.get(n, 0) + 1
                if calls[n] > 1:
                    result['hash'] = '0x' + '00' * 32
            return result
        with self.assertRaises(gate.EvidenceError):
            gate.verify_evidence(self.data, self.fixture['head'], changing_rpc,
                                 lambda repo, commit: True,
                                 lambda addr: self.fixture['sources'][addr.lower()],
                                 self.fixture['artifacts'], self.fixture['historical'])

    def test_head_retreat_revokes_confirmations(self):
        calls = 0
        def retreating_rpc(method, params):
            nonlocal calls
            if method == 'eth_blockNumber':
                calls += 1
                if calls > 1:
                    h = self.data['refund_tx']
                    return self.responses[json.dumps(['eth_getTransactionReceipt', [h]], separators=(',', ':'))]['blockNumber']
            return self.rpc(method, params)
        with self.assertRaises(gate.EvidenceError):
            gate.verify_evidence(self.data, self.fixture['head'], retreating_rpc,
                                 lambda repo, commit: True,
                                 lambda addr: self.fixture['sources'][addr.lower()],
                                 self.fixture['artifacts'], self.fixture['historical'])

    def test_exact_head_ci_is_required(self):
        with self.assertRaises(gate.EvidenceError):
            gate.verify_evidence(self.data, self.fixture['head'], self.rpc,
                                 lambda repo, commit: False,
                                 lambda addr: {}, self.fixture['artifacts'], self.fixture['historical'])

    def test_legacy_commit_fields_are_rejected(self):
        self.data['deployed_source_commit'] = '0' * 40
        self.rejected()
        del self.data['deployed_source_commit']
        self.data['scenarios']['release']['backend_commit'] = '0' * 40
        self.rejected()

    def test_workflow_path_binding(self):
        for path in ('.github/workflows/verify.yml', '.github/workflows/verify.yml@refs/heads/main'):
            self.assertTrue(gate.workflow_path_valid(path))
        for path in ('verify.yml', '.github/workflows/other.yml', '.github/workflows/verify.yml.evil', 'evil.github/workflows/verify.yml'):
            self.assertFalse(gate.workflow_path_valid(path))


if __name__ == '__main__':
    unittest.main()
