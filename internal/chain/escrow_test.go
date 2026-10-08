package chain

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestEscrowIDBindsAllTerms(t *testing.T) {
	t.Parallel()
	var intent [16]byte
	intent[0] = 0x42
	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	payer := common.HexToAddress("0x2000000000000000000000000000000000000002")
	payee := common.HexToAddress("0x3000000000000000000000000000000000000003")
	amount := big.NewInt(1_000_000)
	base, err := EscrowID(31337, contract, intent, payer, payee, amount, 1_800_000_000)
	if err != nil {
		t.Fatal(err)
	}
	variants := []struct {
		chainID                uint64
		contract, payer, payee common.Address
		amount                 *big.Int
		expiry                 uint64
	}{
		{31338, contract, payer, payee, amount, 1_800_000_000},
		{31337, payer, payer, payee, amount, 1_800_000_000},
		{31337, contract, contract, payee, amount, 1_800_000_000},
		{31337, contract, payer, contract, amount, 1_800_000_000},
		{31337, contract, payer, payee, big.NewInt(1_000_001), 1_800_000_000},
		{31337, contract, payer, payee, amount, 1_800_000_001},
	}
	for _, tc := range variants {
		got, err := EscrowID(tc.chainID, tc.contract, intent, tc.payer, tc.payee, tc.amount, tc.expiry)
		if err == nil && got == base {
			t.Fatalf("variant produced original ID: %+v", tc)
		}
	}
	changedIntent := intent
	changedIntent[1] = 1
	other, err := EscrowID(31337, contract, changedIntent, payer, payee, amount, 1_800_000_000)
	if err != nil || other == base {
		t.Fatalf("intent not bound: %s %v", other, err)
	}
}

func TestCalldataSelectors(t *testing.T) {
	t.Parallel()
	var intent [16]byte
	payee := common.HexToAddress("0x3000000000000000000000000000000000000003")
	create, err := CreateCall(intent, payee, big.NewInt(1), 1_800_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(create[:4], ABI().Methods["createAndFund"].ID) {
		t.Fatal("create selector mismatch")
	}
	for _, kind := range []string{"release", "refund"} {
		data, err := OperatorCall(kind, common.HexToHash("0x01"))
		if err != nil || !bytes.Equal(data[:4], ABI().Methods[kind].ID) {
			t.Fatalf("%s selector mismatch: %v", kind, err)
		}
	}
	if _, err := OperatorCall("rescue", common.Hash{}); err == nil {
		t.Fatal("unsupported operator method accepted")
	}
}
