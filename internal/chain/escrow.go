package chain

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const escrowABIJSON = `[
 {"type":"function","name":"createAndFund","inputs":[{"name":"intentId","type":"bytes16"},{"name":"payee","type":"address"},{"name":"amount","type":"uint128"},{"name":"expiresAt","type":"uint64"}],"outputs":[{"name":"escrowId","type":"bytes32"}]},
 {"type":"function","name":"release","inputs":[{"name":"escrowId","type":"bytes32"}],"outputs":[]},
 {"type":"function","name":"refund","inputs":[{"name":"escrowId","type":"bytes32"}],"outputs":[]},
 {"type":"event","name":"EscrowCreated","inputs":[{"name":"escrowId","type":"bytes32","indexed":true},{"name":"payer","type":"address","indexed":true},{"name":"payee","type":"address","indexed":true},{"name":"token","type":"address"},{"name":"amount","type":"uint256"},{"name":"expiresAt","type":"uint64"}]},
 {"type":"event","name":"EscrowFunded","inputs":[{"name":"escrowId","type":"bytes32","indexed":true},{"name":"amount","type":"uint256"}]},
 {"type":"event","name":"EscrowReleased","inputs":[{"name":"escrowId","type":"bytes32","indexed":true},{"name":"payee","type":"address","indexed":true},{"name":"amount","type":"uint256"}]},
 {"type":"event","name":"EscrowRefunded","inputs":[{"name":"escrowId","type":"bytes32","indexed":true},{"name":"payer","type":"address","indexed":true},{"name":"amount","type":"uint256"}]},
 {"type":"event","name":"EscrowExpired","inputs":[{"name":"escrowId","type":"bytes32","indexed":true}]}
]`

var ErrInvalidEscrowTerms = errors.New("invalid escrow terms")

var contractABI = mustABI()
var idArgs = mustIDArgs()

func mustABI() abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(escrowABIJSON))
	if err != nil {
		panic(err)
	}
	return parsed
}

func mustIDArgs() abi.Arguments {
	types := []string{"string", "uint256", "address", "bytes16", "address", "address", "uint128", "uint64"}
	args := make(abi.Arguments, 0, len(types))
	for _, name := range types {
		typ, err := abi.NewType(name, "", nil)
		if err != nil {
			panic(err)
		}
		args = append(args, abi.Argument{Type: typ})
	}
	return args
}

func validAmount(amount *big.Int) bool {
	return amount != nil && amount.Sign() > 0 && amount.BitLen() <= 128
}

func EscrowID(chainID uint64, contract common.Address, intentID [16]byte, payer, payee common.Address, amount *big.Int, expiresAt uint64) (common.Hash, error) {
	if chainID == 0 || contract == (common.Address{}) || payer == (common.Address{}) || payee == (common.Address{}) || payer == payee || payee == contract || !validAmount(amount) || expiresAt == 0 {
		return common.Hash{}, ErrInvalidEscrowTerms
	}
	encoded, err := idArgs.Pack("SETTLEKIT_ESCROW_V1", new(big.Int).SetUint64(chainID), contract, intentID, payer, payee, amount, expiresAt)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode escrow ID: %w", err)
	}
	return crypto.Keccak256Hash(encoded), nil
}

func CreateCall(intentID [16]byte, payee common.Address, amount *big.Int, expiresAt uint64) ([]byte, error) {
	if payee == (common.Address{}) || !validAmount(amount) || expiresAt == 0 {
		return nil, ErrInvalidEscrowTerms
	}
	data, err := contractABI.Pack("createAndFund", intentID, payee, amount, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("encode createAndFund: %w", err)
	}
	return data, nil
}

func OperatorCall(kind string, escrowID common.Hash) ([]byte, error) {
	if kind != "release" && kind != "refund" {
		return nil, fmt.Errorf("unsupported operator call %q: %w", kind, ErrInvalidEscrowTerms)
	}
	data, err := contractABI.Pack(kind, escrowID)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", kind, err)
	}
	return data, nil
}

func ABI() abi.ABI { return contractABI }
