package indexer

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/chain"
	"settlekit/internal/payments"
	"settlekit/internal/store"
)

type fakeSource struct {
	chainID int64
	head    uint64
	blocks  map[uint64]Block
}

func (f *fakeSource) ChainID(context.Context) (*big.Int, error) { return big.NewInt(f.chainID), nil }
func (f *fakeSource) Head(context.Context) (uint64, error)      { return f.head, nil }
func (f *fakeSource) Block(_ context.Context, number uint64) (Block, error) {
	block, ok := f.blocks[number]
	if !ok {
		return Block{}, errors.New("missing fake block")
	}
	return block, nil
}

func indexerDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func fakeHash(marker byte) common.Hash { var h common.Hash; h[31] = marker; return h }

func makeEventLogs(t *testing.T, contract, payer, payee, token common.Address, escrow common.Hash, amount *big.Int, expiry uint64, block Block) []Log {
	t.Helper()
	abi := chain.ABI()
	created, err := abi.Events["EscrowCreated"].Inputs.NonIndexed().Pack(token, amount, expiry)
	if err != nil {
		t.Fatal(err)
	}
	funded, err := abi.Events["EscrowFunded"].Inputs.NonIndexed().Pack(amount)
	if err != nil {
		t.Fatal(err)
	}
	txHash := fakeHash(201)
	return []Log{
		{Address: contract, Topics: []common.Hash{abi.Events["EscrowCreated"].ID, escrow, common.BytesToHash(payer.Bytes()), common.BytesToHash(payee.Bytes())}, Data: created, BlockHash: block.Hash, BlockNumber: block.Number, TxHash: txHash, Index: 0},
		{Address: contract, Topics: []common.Hash{abi.Events["EscrowFunded"].ID, escrow}, Data: funded, BlockHash: block.Hash, BlockNumber: block.Number, TxHash: txHash, Index: 1},
	}
}

func TestFundingTransitionsAndAtomicCheckpoint(t *testing.T) {
	testTerminalReorgRecovery(t, "EscrowReleased", "RELEASED")
}

func TestRefundTerminalReorgRecovery(t *testing.T) {
	testTerminalReorgRecovery(t, "EscrowRefunded", "REFUNDED")
}

func testTerminalReorgRecovery(t *testing.T, eventName, terminalStatus string) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	payer := common.HexToAddress("0x2000000000000000000000000000000000000002")
	payee := common.HexToAddress("0x3000000000000000000000000000000000000003")
	token := common.HexToAddress("0x4000000000000000000000000000000000000004")
	intentBytes, intentID, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	chainID := int64(time.Now().UnixNano() & 0x7fffffffffffffff)
	amount := big.NewInt(1000000)
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	escrow, err := chain.EscrowID(uint64(chainID), contract, intentBytes, payer, payee, amount, uint64(expiry.Unix()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO payment_intents(id,merchant_principal,escrow_id,chain_id,escrow_contract,payer,payee,token,amount,status,expires_at)
		VALUES($1,'merchant',$2,$3,$4,$5,$6,$7,$8,'AWAITING_CHAIN',$9)`, intentID, escrow[:], chainID, contract.Bytes(), payer.Bytes(), payee.Bytes(), token.Bytes(), amount.String(), expiry)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	genesis := Block{Number: 0, Hash: fakeHash(101), Time: base}
	funding := Block{Number: 1, Hash: fakeHash(102), Parent: genesis.Hash, Time: base.Add(time.Second)}
	funding.Logs = makeEventLogs(t, contract, payer, payee, token, escrow, amount, uint64(expiry.Unix()), funding)
	second := Block{Number: 2, Hash: fakeHash(103), Parent: funding.Hash, Time: base.Add(2 * time.Second)}
	third := Block{Number: 3, Hash: fakeHash(104), Parent: second.Hash, Time: base.Add(3 * time.Second)}
	terminal := Block{Number: 4, Hash: fakeHash(105), Parent: third.Hash, Time: base.Add(4 * time.Second)}
	abi := chain.ABI()
	releasedData, err := abi.Events[eventName].Inputs.NonIndexed().Pack(amount)
	if err != nil {
		t.Fatal(err)
	}
	recipient := payee
	if eventName == "EscrowRefunded" {
		recipient = payer
	}
	terminal.Logs = []Log{{Address: contract, Topics: []common.Hash{abi.Events[eventName].ID, escrow, common.BytesToHash(recipient.Bytes())}, Data: releasedData, BlockHash: terminal.Hash, BlockNumber: 4, TxHash: fakeHash(202), Index: 0}}
	fifth := Block{Number: 5, Hash: fakeHash(106), Parent: terminal.Hash, Time: base.Add(5 * time.Second)}
	sixth := Block{Number: 6, Hash: fakeHash(107), Parent: fifth.Hash, Time: base.Add(6 * time.Second)}
	source := &fakeSource{chainID: chainID, head: 1, blocks: map[uint64]Block{0: genesis, 1: funding, 2: second, 3: third}}
	source.blocks[4], source.blocks[5], source.blocks[6] = terminal, fifth, sixth
	watcher, err := New(pool, source, uint64(chainID), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus := func(want string, wantVersion int64) {
		t.Helper()
		var got string
		var version int64
		if err := pool.QueryRow(ctx, `SELECT status,status_version FROM payment_intents WHERE id=$1`, intentID).Scan(&got, &version); err != nil {
			t.Fatal(err)
		}
		if got != want || version != wantVersion {
			t.Fatalf("status=%s version=%d, want %s/%d", got, version, want, wantVersion)
		}
	}
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("OBSERVED", 1)
	source.head = 2
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMING", 2)
	source.head = 3
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMED", 3)
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMED", 3)
	source.head = 4
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMED", 3)
	source.head = 5
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMED", 3)
	source.head = 6
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(terminalStatus, 4)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE escrow_id=$1`, escrow[:]).Scan(&count); err != nil || count != 3 {
		t.Fatalf("logs=%d err=%v", count, err)
	}
	bad := Block{Number: 7, Hash: fakeHash(108), Parent: fakeHash(244), Time: base.Add(7 * time.Second)}
	source.blocks[7], source.head = bad, 7
	if err := watcher.RunOnce(ctx); !errors.Is(err, ErrCanonicalDiscontinuity) {
		t.Fatalf("bad parent error=%v", err)
	}
	var checkpoint int64
	if err := pool.QueryRow(ctx, `SELECT canonical_head_number FROM sync_state WHERE chain_id=$1`, chainID).Scan(&checkpoint); err != nil || checkpoint != 6 {
		t.Fatalf("checkpoint=%d err=%v", checkpoint, err)
	}
	var originalTerminalEvent string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM outbox_events WHERE intent_id=$1 AND status_version=4`, intentID).Scan(&originalTerminalEvent); err != nil {
		t.Fatal(err)
	}
	originalBranch := make(map[uint64]Block)
	for n, b := range source.blocks {
		originalBranch[n] = b
	}
	newParent := genesis.Hash
	for number := uint64(1); number <= 6; number++ {
		branch := Block{Number: number, Hash: fakeHash(byte(110 + number)), Parent: newParent, Time: base.Add(time.Duration(number) * time.Second)}
		if number == 2 {
			branch.Logs = makeEventLogs(t, contract, payer, payee, token, escrow, amount, uint64(expiry.Unix()), branch)
		}
		source.blocks[number] = branch
		newParent = branch.Hash
	}
	source.head = 6
	shallow, err := New(pool, source, uint64(chainID), contract, 3, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := shallow.RunOnce(ctx); !errors.Is(err, ErrCanonicalDiscontinuity) {
		t.Fatalf("deep reorg not rejected: %v", err)
	}
	assertStatus(terminalStatus, 4)
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatalf("reconcile branch: %v", err)
	}
	assertStatus("AWAITING_CHAIN", 6)
	var reversal []byte
	if err := pool.QueryRow(ctx, `SELECT body FROM outbox_events WHERE intent_id=$1 AND kind='PAYMENT_REVERSED'`, intentID).Scan(&reversal); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(reversal, []byte(originalTerminalEvent)) {
		t.Fatalf("reversal does not reference terminal event: %s", reversal)
	}
	watcher, err = New(pool, source, uint64(chainID), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatalf("replay branch after restart: %v", err)
	}
	assertStatus("CONFIRMED", 9)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE escrow_id=$1 AND NOT removed`, escrow[:]).Scan(&count); err != nil || count != 2 {
		t.Fatalf("canonical logs=%d err=%v", count, err)
	}
	source.blocks = originalBranch
	watcher, err = New(pool, source, uint64(chainID), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("AWAITING_CHAIN", 11)
	watcher, err = New(pool, source, uint64(chainID), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(terminalStatus, 15)
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(terminalStatus, 15)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE escrow_id=$1`, escrow[:]).Scan(&count); err != nil || count != 5 {
		t.Fatalf("retained audit logs=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE escrow_id=$1 AND NOT removed`, escrow[:]).Scan(&count); err != nil || count != 3 {
		t.Fatalf("restored canonical logs=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_state_history WHERE intent_id=$1`, intentID).Scan(&count); err != nil || count != 15 {
		t.Fatalf("state history retained=%d err=%v", count, err)
	}

	source.head = 4
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("CONFIRMED", 17)
	source.head = 6
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(terminalStatus, 18)
	source.head = 1
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus("OBSERVED", 20)
	source.head = 6
	if err := watcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(terminalStatus, 23)

}
