package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"settlekit/internal/chain"
	"settlekit/internal/payments"
	"settlekit/internal/store"
	"testing"
	"time"
)

func TestRecanonicalizePreviouslySeenBlock(t *testing.T) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := uint64(time.Now().UnixNano())
	c := common.HexToAddress("0x1000000000000000000000000000000000000001")
	base := Block{Number: 0, Hash: fakeHash(1), Time: time.Now()}
	a := Block{Number: 1, Hash: fakeHash(2), Parent: base.Hash, Time: base.Time.Add(time.Second)}
	b := Block{Number: 1, Hash: fakeHash(3), Parent: base.Hash, Time: base.Time.Add(time.Second)}
	source := &fakeSource{chainID: int64(id), head: 1, blocks: map[uint64]Block{0: base, 1: a}}
	w, err := New(pool, source, id, c, 2, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	source.blocks[1] = b
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	source.blocks[1] = a
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatalf("A -> B -> A shallow reorg cannot re-adopt known canonical block: %v", err)
	}
}
func TestReorgWith1001UnaffectedIntents(t *testing.T) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := uint64(time.Now().UnixNano())
	c := common.HexToAddress("0x1000000000000000000000000000000000000001")
	base := Block{Number: 0, Hash: fakeHash(11), Time: time.Now()}
	a := Block{Number: 1, Hash: fakeHash(12), Parent: base.Hash, Time: base.Time.Add(time.Second)}
	b := Block{Number: 1, Hash: fakeHash(13), Parent: base.Hash, Time: base.Time.Add(time.Second)}
	source := &fakeSource{chainID: int64(id), head: 1, blocks: map[uint64]Block{0: base, 1: a}}
	w, err := New(pool, source, id, c, 2, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO payment_intents(id,merchant_principal,escrow_id,chain_id,escrow_contract,payer,payee,token,amount,status,expires_at)
 SELECT gen_random_uuid(),'merchant',sha256(convert_to($1::bigint::text||':'||n::text,'UTF8')),$1,$2,decode(repeat('02',20),'hex'),decode(repeat('03',20),'hex'),decode(repeat('04',20),'hex'),1,'AWAITING_CHAIN',date_trunc('second',now()+interval '1 hour') FROM generate_series(1,1001)n`, int64(id), c.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	source.blocks[1] = b
	if err = w.RunOnce(ctx); err != nil {
		t.Fatalf("one-block fork with zero affected intents was refused because 1001 unrelated intents exist: %v", err)
	}
}

func TestRecanonicalizationRejectsChangedIdentity(t *testing.T) {
	for _, mutation := range []string{"number", "parent", "missing-log", "extra-log", "payload", "transaction"} {
		t.Run(mutation, func(t *testing.T) {
			pool := indexerDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			id := uint64(time.Now().UnixNano())
			contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
			base := Block{Number: 0, Hash: fakeHash(1), Time: time.Now()}
			a := Block{Number: 1, Hash: fakeHash(2), Parent: base.Hash, Time: base.Time.Add(time.Second)}
			a.Logs = []Log{{Address: contract, BlockNumber: 1, BlockHash: a.Hash, TxHash: fakeHash(4), Index: 0, Topics: []common.Hash{chain.ABI().Events["EscrowExpired"].ID, fakeHash(9)}}}
			b := Block{Number: 1, Hash: fakeHash(3), Parent: base.Hash, Time: a.Time}
			src := &fakeSource{chainID: int64(id), head: 1, blocks: map[uint64]Block{0: base, 1: a}}
			w, err := New(pool, src, id, contract, 3, 0, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			src.blocks[1] = b
			if err = w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			bad := a
			bad.Logs = append([]Log(nil), a.Logs...)
			switch mutation {
			case "number":
				bad.Number = 2
			case "parent":
				bad.Parent = fakeHash(99)
			case "missing-log":
				bad.Logs = nil
			case "extra-log":
				extra := bad.Logs[0]
				extra.Index = 1
				bad.Logs = append(bad.Logs, extra)
			case "payload":
				bad.Logs[0].Data = []byte{1}
			case "transaction":
				bad.Logs[0].TxHash = fakeHash(98)
			}
			if err = w.commitBlock(ctx, bad, 0, base.Hash, true); err == nil {
				t.Fatal("changed immutable block/log accepted")
			}
			var canonical bool
			var count int
			if err = pool.QueryRow(ctx, `SELECT canonical FROM chain_blocks WHERE chain_id=$1 AND block_hash=$2`, int64(id), a.Hash[:]).Scan(&canonical); err != nil || canonical {
				t.Fatalf("failed replay committed flags: %v %v", canonical, err)
			}
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE chain_id=$1 AND block_hash=$2 AND removed`, int64(id), a.Hash[:]).Scan(&count); err != nil || count != 1 {
				t.Fatalf("audit rows changed: %d %v", count, err)
			}
			src.blocks[1] = a
			w, err = New(pool, src, id, contract, 3, 0, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.RunOnce(ctx); err != nil {
				t.Fatalf("restart could not recover original evidence: %v", err)
			}
		})
	}
}

func TestInterruptedReorgTransactionRestarts(t *testing.T) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := time.Now().UnixNano()
	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	base := Block{Number: 0, Hash: fakeHash(1), Time: time.Now()}
	a := Block{Number: 1, Hash: fakeHash(2), Parent: base.Hash, Time: base.Time.Add(time.Second)}
	src := &fakeSource{chainID: id, head: 1, blocks: map[uint64]Block{0: base, 1: a}}
	w, err := New(pool, src, uint64(id), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("reorg_fault_%d", id)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.chain_id=%d AND NEW.canonical_head_number<OLD.canonical_head_number THEN RAISE EXCEPTION 'injected reorg interruption'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER %s BEFORE UPDATE ON sync_state FOR EACH ROW EXECUTE FUNCTION %s()`, name, id, name, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_, _ = pool.Exec(cleanup, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON sync_state; DROP FUNCTION IF EXISTS %s()`, name, name))
	}()
	src.blocks[1] = Block{Number: 1, Hash: fakeHash(3), Parent: base.Hash, Time: a.Time}
	if err = w.RunOnce(ctx); err == nil {
		t.Fatal("expected transaction fault")
	}
	var canonical bool
	if err = pool.QueryRow(ctx, `SELECT canonical FROM chain_blocks WHERE chain_id=$1 AND block_hash=$2`, id, a.Hash[:]).Scan(&canonical); err != nil || !canonical {
		t.Fatalf("rollback lost old branch: %v %v", canonical, err)
	}
	if _, err = pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON sync_state; DROP FUNCTION %s()`, name, name)); err != nil {
		t.Fatal(err)
	}
	w, err = New(pool, src, uint64(id), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	n, h, ok, err := w.checkpoint(ctx)
	if err != nil || !ok || n != 1 || h != fakeHash(3) {
		t.Fatalf("restarted checkpoint: %d %v %v", n, ok, err)
	}
}

func TestReorgBoundRejects1001AffectedExpiriesAtomically(t *testing.T) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := time.Now().UnixNano()
	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	base := Block{Number: 0, Hash: fakeHash(1), Time: time.Now()}
	a := Block{Number: 1, Hash: fakeHash(2), Parent: base.Hash, Time: base.Time.Add(2 * time.Hour)}
	src := &fakeSource{chainID: id, head: 1, blocks: map[uint64]Block{0: base, 1: a}}
	w, err := New(pool, src, uint64(id), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO payment_intents(id,merchant_principal,escrow_id,chain_id,escrow_contract,payer,payee,token,amount,status,expires_at)
 SELECT gen_random_uuid(),'test',sha256(convert_to($1::bigint::text||':'||n::text,'UTF8')),$1,$2,decode(repeat('02',20),'hex'),decode(repeat('03',20),'hex'),decode(repeat('04',20),'hex'),1,'EXPIRED',date_trunc('second',now()+interval '1 hour') FROM generate_series(1,1001)n`, id, contract.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	src.blocks[1] = Block{Number: 1, Hash: fakeHash(3), Parent: base.Hash, Time: a.Time}
	if err = w.RunOnce(ctx); !errors.Is(err, ErrCanonicalDiscontinuity) {
		t.Fatalf("safety bound bypassed: %v", err)
	}
	n, h, ok, err := w.checkpoint(ctx)
	if err != nil || !ok || n != 1 || h != a.Hash {
		t.Fatalf("failed bound partially committed: %d %v %v", n, ok, err)
	}
}

func TestReorgExpiryRecomputesWithoutFunding(t *testing.T) {
	pool := indexerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := time.Now().UnixNano()
	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	_, intent, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(intent))
	baseTime := time.Now().Truncate(time.Second)
	expiry := baseTime.Add(time.Hour)
	_, err = store.CreateIntent(ctx, pool, store.Idempotency{Principal: "test", Method: "POST", Route: "/v1/payment-intents", Key: intent, Hash: hash}, store.Intent{ID: intent, MerchantPrincipal: "test", EscrowID: hash[:], ChainID: id, EscrowContract: contract.Bytes(), Payer: common.HexToAddress("0x2").Bytes(), Payee: common.HexToAddress("0x3").Bytes(), Token: common.HexToAddress("0x4").Bytes(), Amount: "1", Status: "AWAITING_CHAIN", ExpiresAt: expiry}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	base := Block{Number: 0, Hash: fakeHash(1), Time: baseTime}
	a := Block{Number: 1, Hash: fakeHash(2), Parent: base.Hash, Time: expiry.Add(time.Second)}
	b := Block{Number: 1, Hash: fakeHash(3), Parent: base.Hash, Time: baseTime.Add(time.Minute)}
	src := &fakeSource{chainID: id, head: 1, blocks: map[uint64]Block{0: base, 1: a}}
	w, err := New(pool, src, uint64(id), contract, 3, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT status FROM payment_intents WHERE id=$1`, intent).Scan(&got); err != nil || got != want {
			t.Fatalf("status=%s want=%s err=%v", got, want, err)
		}
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("EXPIRED")
	src.blocks[1] = b
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("AWAITING_CHAIN")
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("AWAITING_CHAIN")
	src.blocks[1] = a
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("EXPIRED")
	var history, reversals int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM payment_state_history WHERE intent_id=$1`, intent).Scan(&history); err != nil || history != 5 {
		t.Fatalf("history=%d err=%v", history, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND kind='PAYMENT_REVERSED'`, intent).Scan(&reversals); err != nil || reversals != 1 {
		t.Fatalf("reversals=%d err=%v", reversals, err)
	}
}
