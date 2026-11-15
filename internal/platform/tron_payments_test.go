package platform

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"reladock/internal/tronpayments"
)

const testTronCollectionAddress = "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw"

func TestTronPaymentMigration36SchemaAndReceiptImmutability(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if SchemaVersion < 36 {
		t.Fatalf("SchemaVersion=%d want at least 36", SchemaVersion)
	}
	var version int
	var orders, receipts bool
	if err := s.pool.QueryRow(ctx, `SELECT max(version),to_regclass('tron_payment_orders') IS NOT NULL,to_regclass('tron_payment_receipts') IS NOT NULL FROM schema_migrations`).Scan(&version, &orders, &receipts); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || !orders || !receipts {
		t.Fatalf("version=%d orders=%v receipts=%v", version, orders, receipts)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}

	who, _ := register(t, s, "tron-migration@example.com")
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-schema-order", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO tron_payment_receipts(tx_hash,order_id,tenant_id,log_index,from_address,to_address,contract_address,amount_base_units,block_number,block_time) VALUES($1,$2,$3,0,$4,$5,$6,$7,1,clock_timestamp())`, strings.Repeat("a", 64), order.ID, who.TenantID, testTronCollectionAddress, testTronCollectionAddress, testTronCollectionAddress, order.AmountBaseUnits)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE tron_payment_receipts SET log_index=1`,
		`DELETE FROM tron_payment_receipts`,
		`TRUNCATE tron_payment_receipts`,
	} {
		if _, err = s.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("immutable receipt accepted %q", statement)
		}
	}
}

func TestTronScanMigration38CursorInvariants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if SchemaVersion < 38 {
		t.Fatalf("SchemaVersion=%d want at least 38", SchemaVersion)
	}
	var version int
	var cursors bool
	if err := s.pool.QueryRow(ctx, `SELECT max(version),to_regclass('tron_payment_scan_cursors') IS NOT NULL FROM schema_migrations`).Scan(&version, &cursors); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || !cursors {
		t.Fatalf("version=%d cursors=%v", version, cursors)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	first := newID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO tron_payment_scan_cursors(id,network,contract_address,collection_address,next_block) VALUES($1,'TRON',$2,$3,100)`, first, tronpayments.OfficialUSDTAddress, testTronCollectionAddress); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"duplicate tuple": `INSERT INTO tron_payment_scan_cursors(id,network,contract_address,collection_address,next_block) VALUES('00000000-0000-0000-0000-000000000101','TRON','` + tronpayments.OfficialUSDTAddress + `','` + testTronCollectionAddress + `',101)`,
		"half lease":      `UPDATE tron_payment_scan_cursors SET lease_token='00000000-0000-0000-0000-000000000102',lease_expires_at=NULL WHERE id='` + first + `'`,
		"long error":      `UPDATE tron_payment_scan_cursors SET last_error=repeat('x',121) WHERE id='` + first + `'`,
		"zero height":     `UPDATE tron_payment_scan_cursors SET next_block=0 WHERE id='` + first + `'`,
	} {
		if _, err := s.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s invariant accepted", name)
		}
	}
}

func claimedTronOrder(t *testing.T, s *Store, who Identity, key string, amount int64, hash string) TronPaymentOrder {
	t.Helper()
	ctx := context.Background()
	order, err := s.CreateTronPaymentOrder(ctx, who, key, testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: amount})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, order.ID, hash); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || claimed == nil || claimed.ID != order.ID {
		t.Fatalf("worker claim=%+v err=%v", claimed, err)
	}
	return *claimed
}

func matchingTronReceipt(order TronPaymentOrder) tronpayments.Receipt {
	return tronpayments.Receipt{
		TxHash:          order.TxHash,
		BlockNumber:     123,
		BlockTime:       order.CreatedAt.Add(time.Minute),
		LogIndex:        2,
		FromAddress:     "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		ToAddress:       order.CollectionAddress,
		ContractAddress: tronpayments.OfficialUSDTAddress,
		AmountBaseUnits: order.AmountBaseUnits,
	}
}

func discoveredTronReceipt(order TronPaymentOrder, hash string, blockTime time.Time) tronpayments.Receipt {
	return tronpayments.Receipt{
		TxHash:          hash,
		BlockNumber:     76543210,
		BlockTime:       blockTime,
		LogIndex:        3,
		FromAddress:     "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
		ToAddress:       order.CollectionAddress,
		ContractAddress: tronpayments.OfficialUSDTAddress,
		AmountBaseUnits: order.AmountBaseUnits,
	}
}

func TestTronScanCursorRetriesSameHeightAndFencesStaleLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lease, err := s.ClaimNextTronScanBlock(ctx, testTronCollectionAddress, 5000, 1200, time.Minute)
	if err != nil || lease == nil || lease.Height != 3800 || lease.Token == "" {
		t.Fatalf("first lease=%+v err=%v", lease, err)
	}
	blocked, err := s.ClaimNextTronScanBlock(ctx, testTronCollectionAddress, 5000, 1200, time.Minute)
	if err != nil || blocked != nil {
		t.Fatalf("concurrent lease=%+v err=%v", blocked, err)
	}
	stale := *lease
	stale.Token = newID()
	if err = s.CompleteTronScanBlock(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion err=%v", err)
	}
	if err = s.RetryTronScanBlock(ctx, *lease, "provider-unavailable", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE tron_payment_scan_cursors SET next_attempt_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimNextTronScanBlock(ctx, testTronCollectionAddress, 5000, 1200, time.Minute)
	if err != nil || retry == nil || retry.Height != lease.Height || retry.Token == lease.Token {
		t.Fatalf("retry lease=%+v err=%v", retry, err)
	}
	if err = s.CompleteTronScanBlock(ctx, *retry); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimNextTronScanBlock(ctx, testTronCollectionAddress, 5000, 1200, time.Minute)
	if err != nil || next == nil || next.Height != retry.Height+1 {
		t.Fatalf("next lease=%+v err=%v", next, err)
	}
}

func TestTronScanCursorAllowsOnlyOneConcurrentScanner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan *TronScanLease, 2)
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			lease, err := s.ClaimNextTronScanBlock(ctx, testTronCollectionAddress, 9000, 1200, time.Minute)
			results <- lease
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)
	claimed := 0
	for lease := range results {
		if lease != nil {
			claimed++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d want 1", claimed)
	}
}

func TestTronAutoMatchAttachesCandidateWithoutCreditingWallet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-auto@example.com")
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-auto-order", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	receipt := discoveredTronReceipt(order, strings.Repeat("b", 64), order.CreatedAt.Add(time.Minute))
	matched, err := s.MatchDiscoveredTronPayment(ctx, receipt)
	if err != nil || matched == nil || matched.ID != order.ID || matched.State != "confirming" || matched.TxHash != receipt.TxHash {
		t.Fatalf("matched=%+v err=%v", matched, err)
	}
	replayed, err := s.MatchDiscoveredTronPayment(ctx, receipt)
	if err != nil || replayed == nil || replayed.ID != order.ID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	var balance, receipts, audits int64
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1),0),(SELECT count(*) FROM tron_payment_receipts WHERE order_id=$2),(SELECT count(*) FROM platform_audit_logs WHERE action='tron-payment.auto-claim' AND resource_id=$3)`, who.TenantID, order.ID, order.ID).Scan(&balance, &receipts, &audits); err != nil {
		t.Fatal(err)
	}
	if balance != 0 || receipts != 0 || audits != 1 {
		t.Fatalf("balance=%d receipts=%d audits=%d", balance, receipts, audits)
	}
}

func TestTronAutoMatchRejectsOutOfWindowAndRollsBackAuditFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-auto-window@example.com")
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-auto-window", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 600})
	if err != nil {
		t.Fatal(err)
	}
	outOfWindow := discoveredTronReceipt(order, strings.Repeat("c", 64), order.ExpiresAt.Add(time.Second))
	matched, err := s.MatchDiscoveredTronPayment(ctx, outOfWindow)
	if err != nil || matched != nil {
		t.Fatalf("out-of-window matched=%+v err=%v", matched, err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_tron_auto_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='tron-payment.auto-claim' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_tron_auto_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_tron_auto_audit()`); err != nil {
		t.Fatal(err)
	}
	valid := discoveredTronReceipt(order, strings.Repeat("d", 64), order.CreatedAt.Add(time.Minute))
	if _, err = s.MatchDiscoveredTronPayment(ctx, valid); err == nil {
		t.Fatal("audit failure accepted")
	}
	current, err := s.GetTronPaymentOrder(ctx, who, order.ID)
	if err != nil || current.State != "pending" || current.TxHash != "" {
		t.Fatalf("failed audit leaked order=%+v err=%v", current, err)
	}
}

func TestTronAutoMatchRecoversInWindowTransferAfterClaimGrace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-auto-recovery@example.com")
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-auto-recovery", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 650})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `UPDATE tron_payment_orders SET created_at=clock_timestamp()-interval '26 hours',expires_at=clock_timestamp()-interval '25 hours',state='expired' WHERE id=$1 RETURNING created_at,expires_at`, order.ID).Scan(&order.CreatedAt, &order.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	receipt := discoveredTronReceipt(order, strings.Repeat("8", 64), order.CreatedAt.Add(time.Minute))
	matched, err := s.MatchDiscoveredTronPayment(ctx, receipt)
	if err != nil || matched == nil || matched.ID != order.ID || matched.State != "confirming" {
		t.Fatalf("recovered=%+v err=%v", matched, err)
	}
}

func TestTronAutoAndManualClaimSameHashConverge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-auto-race@example.com")
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-auto-race", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 700})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("e", 64)
	receipt := discoveredTronReceipt(order, hash, order.CreatedAt.Add(time.Minute))
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, claimErr := s.ClaimTronPaymentOrder(ctx, who, order.ID, hash)
		errs <- claimErr
	}()
	go func() {
		<-start
		_, claimErr := s.MatchDiscoveredTronPayment(ctx, receipt)
		errs <- claimErr
	}()
	close(start)
	for range 2 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.GetTronPaymentOrder(ctx, who, order.ID)
	if err != nil || current.State != "confirming" || current.TxHash != hash {
		t.Fatalf("current=%+v err=%v", current, err)
	}
}

func TestTronPaymentCreditIsAtomicAndIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-credit@example.com")
	order := claimedTronOrder(t, s, who, "tron-credit-order", 1234, strings.Repeat("d", 64))
	receipt := matchingTronReceipt(order)
	credited, err := s.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if credited.State != "credited" || credited.CreditLedgerID == 0 {
		t.Fatalf("credited=%+v", credited)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != order.AmountCents {
		t.Fatalf("wallet=%+v err=%v", wallet, err)
	}
	replayed, err := s.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, receipt)
	if err != nil || replayed.CreditLedgerID != credited.CreditLedgerID {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	var ledger, receipts int
	if err = s.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM billing_ledger WHERE tenant_id=$1 AND kind='payment-credit'),(SELECT count(*) FROM tron_payment_receipts WHERE order_id=$2)", who.TenantID, order.ID).Scan(&ledger, &receipts); err != nil || ledger != 1 || receipts != 1 {
		t.Fatalf("ledger=%d receipts=%d err=%v", ledger, receipts, err)
	}
}

func TestTronPaymentCreditConcurrentReplayCreditsOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-race@example.com")
	order := claimedTronOrder(t, s, who, "tron-race-order", 500, strings.Repeat("e", 64))
	receipt := matchingTronReceipt(order)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, receipt)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply: %v", err)
		}
	}
	var balance int64
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT balance_cents,(SELECT count(*) FROM billing_ledger WHERE tenant_id=$1 AND kind='payment-credit') FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balance, &count); err != nil || balance != 500 || count != 1 {
		t.Fatalf("balance=%d count=%d err=%v", balance, count, err)
	}
}

func TestTronPaymentCreditMismatchGoesToReviewWithoutWalletMutation(t *testing.T) {
	for name, tc := range map[string]struct {
		hash   string
		mutate func(*tronpayments.Receipt)
	}{
		"amount":    {hash: strings.Repeat("2", 64), mutate: func(r *tronpayments.Receipt) { r.AmountBaseUnits++ }},
		"recipient": {hash: strings.Repeat("3", 64), mutate: func(r *tronpayments.Receipt) { r.ToAddress = r.FromAddress }},
		"contract":  {hash: strings.Repeat("4", 64), mutate: func(r *tronpayments.Receipt) { r.ContractAddress = r.FromAddress }},
		"time":      {hash: strings.Repeat("5", 64), mutate: func(r *tronpayments.Receipt) { r.BlockTime = time.Time{} }},
	} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			who, _ := register(t, s, "tron-review-"+name+"@example.com")
			order := claimedTronOrder(t, s, who, "tron-review-"+name, 500, tc.hash)
			receipt := matchingTronReceipt(order)
			tc.mutate(&receipt)
			reviewed, err := s.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, receipt)
			if err != nil || reviewed.State != "needs-review" || reviewed.LastError != "receipt-mismatch" {
				t.Fatalf("reviewed=%+v err=%v", reviewed, err)
			}
			wallet, err := s.Wallet(ctx, who)
			if err != nil || wallet.BalanceCents != 0 {
				t.Fatalf("wallet=%+v err=%v", wallet, err)
			}
		})
	}
}

func TestTronPaymentCreditAllowsFrozenWalletAndRejectsOverflow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-frozen@example.com")
	if _, err := s.pool.Exec(ctx, "INSERT INTO billing_wallets(tenant_id,spending_frozen) VALUES($1,true)", who.TenantID); err != nil {
		t.Fatal(err)
	}
	order := claimedTronOrder(t, s, who, "tron-frozen-order", 500, strings.Repeat("f", 64))
	if _, err := s.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, matchingTronReceipt(order)); err != nil {
		t.Fatalf("frozen wallet credit: %v", err)
	}
	var balance int64
	if err := s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balance); err != nil || balance != 500 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}

	overflow := claimedTronOrder(t, s, who, "tron-overflow-order", 500, strings.Repeat("1", 64))
	if _, err := s.pool.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", who.TenantID, int64(math.MaxInt64-100)); err != nil {
		t.Fatal(err)
	}
	reviewed, err := s.ApplyTronPaymentReceipt(ctx, overflow.ID, overflow.LeaseToken, matchingTronReceipt(overflow))
	if err != nil || reviewed.State != "needs-review" || reviewed.LastError != "wallet-overflow" {
		t.Fatalf("overflow=%+v err=%v", reviewed, err)
	}
}

func TestTronPaymentPlanPurchaseRunsAfterCredit(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-plan-credit", testTronCollectionAddress, CreateTronPaymentOrderInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, order.ID, strings.Repeat("6", 64)); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	credited, err := s.ApplyTronPaymentReceipt(ctx, claimed.ID, claimed.LeaseToken, matchingTronReceipt(*claimed))
	if err != nil || credited.PurchaseState != "pending" {
		t.Fatalf("credit=%+v err=%v", credited, err)
	}
	recovered, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || recovered == nil || recovered.ID != credited.ID || recovered.State != "credited" {
		t.Fatalf("post-credit recovery=%+v err=%v", recovered, err)
	}
	if err = s.CompleteTronPlanPurchase(ctx, recovered.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := s.GetTronPaymentOrder(ctx, who, credited.ID)
	if err != nil || completed.PurchaseState != "purchased" || completed.SubscriptionID == "" {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 0 {
		t.Fatalf("wallet=%+v err=%v", wallet, err)
	}
}

func TestTronPaymentPlanFailureRetainsCredit(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-plan-fallback", testTronCollectionAddress, CreateTronPaymentOrderInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, order.ID, strings.Repeat("7", 64)); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	credited, err := s.ApplyTronPaymentReceipt(ctx, claimed.ID, claimed.LeaseToken, matchingTronReceipt(*claimed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET status='maintenance' WHERE id=$1", rule.NodeID); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteTronPlanPurchase(ctx, credited.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := s.GetTronPaymentOrder(ctx, who, credited.ID)
	if err != nil || failed.PurchaseState != "failed" || failed.PurchaseError == "" || failed.SubscriptionID != "" {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != plan.PriceCents {
		t.Fatalf("wallet=%+v want=%d err=%v", wallet, plan.PriceCents, err)
	}
}

func TestTronPaymentOrderTopUpIdempotencyAndClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-topup@example.com")
	input := CreateTronPaymentOrderInput{AmountCents: 1234}
	first, err := s.CreateTronPaymentOrder(ctx, who, "tron-topup-order", testTronCollectionAddress, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTronPaymentOrder(ctx, who, "tron-topup-order", testTronCollectionAddress, input)
	if err != nil || second.ID != first.ID {
		t.Fatalf("replay=%+v err=%v", second, err)
	}
	if first.Purpose != "wallet-topup" || first.AmountCents != 1234 || first.AmountBaseUnits < 12340000 || first.AmountBaseUnits >= 12350000 || first.CollectionAddress != testTronCollectionAddress || first.State != "pending" || first.PurchaseState != "none" {
		t.Fatalf("order=%+v", first)
	}
	if _, err = s.CreateTronPaymentOrder(ctx, who, "tron-topup-order", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 1235}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	if _, err = s.CreateTronPaymentOrder(ctx, who, "tron-too-small", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 49}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("small amount err=%v", err)
	}

	txHash := strings.Repeat("a", 64)
	claimed, err := s.ClaimTronPaymentOrder(ctx, who, first.ID, txHash)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != "confirming" || claimed.TxHash != txHash || claimed.ClaimedAt == nil {
		t.Fatalf("claimed=%+v", claimed)
	}
	replayed, err := s.ClaimTronPaymentOrder(ctx, who, first.ID, txHash)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("claim replay=%+v err=%v", replayed, err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, first.ID, strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("different hash err=%v", err)
	}
}

func TestTronPaymentOrderUsesServerPlanPriceAndTenantScope(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	order, err := s.CreateTronPaymentOrder(ctx, who, "tron-plan-order", testTronCollectionAddress, CreateTronPaymentOrderInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if order.Purpose != "plan-purchase" || order.AmountCents != plan.PriceCents || order.AmountBaseUnits < plan.PriceCents*10000 || order.AmountBaseUnits >= (plan.PriceCents+1)*10000 || order.RuleID != rule.ID || order.PriceVersionID != plan.ID || order.PurchaseState != "pending" {
		t.Fatalf("order=%+v plan=%+v", order, plan)
	}
	if _, err = s.CreateTronPaymentOrder(ctx, who, "tron-plan-forged", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 1, PriceVersionID: plan.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed quote err=%v", err)
	}
	other, _ := register(t, s, "tron-other@example.com")
	if _, err = s.CreateTronPaymentOrder(ctx, other, "tron-cross-rule", testTronCollectionAddress, CreateTronPaymentOrderInput{RuleID: rule.ID, PriceVersionID: plan.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant rule err=%v", err)
	}
	if _, err = s.GetTronPaymentOrder(ctx, other, order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant read err=%v", err)
	}
	listed, err := s.ListTronPaymentOrders(ctx, who)
	if err != nil || len(listed) != 1 || listed[0].ID != order.ID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestTronPaymentOrderWorkerLeaseIsFenced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-unique@example.com")
	a, err := s.CreateTronPaymentOrder(ctx, who, "tron-unique-a", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	if _, err = s.ClaimTronPaymentOrder(ctx, who, a.ID, hash); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || claimed == nil || claimed.ID != a.ID || claimed.LeaseToken == "" {
		t.Fatalf("worker claim=%+v err=%v", claimed, err)
	}
	none, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || none != nil {
		t.Fatalf("duplicate worker claim=%+v err=%v", none, err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE tron_payment_orders SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || reclaimed == nil || reclaimed.ID != a.ID || reclaimed.LeaseToken == claimed.LeaseToken {
		t.Fatalf("reclaimed=%+v err=%v", reclaimed, err)
	}
	if _, err = s.ApplyTronPaymentReceipt(ctx, a.ID, claimed.LeaseToken, matchingTronReceipt(*reclaimed)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired lease applied err=%v", err)
	}
}

func TestTronPaymentCrossTenantHashClaimCannotStealOrBlockCredit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	victim, _ := register(t, s, "tron-victim@example.com")
	attacker, _ := register(t, s, "tron-attacker@example.com")
	victimOrder, err := s.CreateTronPaymentOrder(ctx, victim, "tron-victim-order", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	attackerOrder, err := s.CreateTronPaymentOrder(ctx, attacker, "tron-attacker-order", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	if victimOrder.AmountBaseUnits == attackerOrder.AmountBaseUnits {
		t.Fatalf("active cross-tenant quotes collided: %d", victimOrder.AmountBaseUnits)
	}
	hash := strings.Repeat("8", 64)
	if _, err = s.ClaimTronPaymentOrder(ctx, attacker, attackerOrder.ID, hash); err != nil {
		t.Fatalf("attacker claim: %v", err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, victim, victimOrder.ID, hash); err != nil {
		t.Fatalf("victim claim blocked: %v", err)
	}
	leases := map[string]TronPaymentOrder{}
	for range 2 {
		claimed, claimErr := s.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
		if claimErr != nil || claimed == nil {
			t.Fatalf("claim=%+v err=%v", claimed, claimErr)
		}
		leases[claimed.ID] = *claimed
	}
	victimLease := leases[victimOrder.ID]
	attackerLease := leases[attackerOrder.ID]
	receipt := matchingTronReceipt(victimLease)
	if reviewed, applyErr := s.ApplyTronPaymentReceipt(ctx, attackerOrder.ID, attackerLease.LeaseToken, receipt); applyErr != nil || reviewed.State != "needs-review" {
		t.Fatalf("attacker apply=%+v err=%v", reviewed, applyErr)
	}
	credited, err := s.ApplyTronPaymentReceipt(ctx, victimOrder.ID, victimLease.LeaseToken, receipt)
	if err != nil || credited.State != "credited" {
		t.Fatalf("victim credit=%+v err=%v", credited, err)
	}
	victimWallet, err := s.Wallet(ctx, victim)
	if err != nil || victimWallet.BalanceCents != 500 {
		t.Fatalf("victim wallet=%+v err=%v", victimWallet, err)
	}
	attackerWallet, err := s.Wallet(ctx, attacker)
	if err != nil || attackerWallet.BalanceCents != 0 {
		t.Fatalf("attacker wallet=%+v err=%v", attackerWallet, err)
	}
}

func TestTronPaymentExpiredDisplayKeepsBoundedLateClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "tron-expiry@example.com")
	late, err := s.CreateTronPaymentOrder(ctx, who, "tron-expiry-late", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 700})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE tron_payment_orders SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1", late.ID); err != nil {
		t.Fatal(err)
	}
	visible, err := s.GetTronPaymentOrder(ctx, who, late.ID)
	if err != nil || visible.State != "expired" || !visible.ClaimAllowed {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, late.ID, strings.Repeat("9", 64)); err != nil {
		t.Fatalf("late in-window transfer hash rejected: %v", err)
	}

	closed, err := s.CreateTronPaymentOrder(ctx, who, "tron-expiry-closed", testTronCollectionAddress, CreateTronPaymentOrderInput{AmountCents: 800})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE tron_payment_orders SET created_at=clock_timestamp()-interval '26 hours',expires_at=clock_timestamp()-interval '25 hours' WHERE id=$1", closed.ID); err != nil {
		t.Fatal(err)
	}
	visible, err = s.GetTronPaymentOrder(ctx, who, closed.ID)
	if err != nil || visible.State != "expired" || visible.ClaimAllowed {
		t.Fatalf("closed visible=%+v err=%v", visible, err)
	}
	if _, err = s.ClaimTronPaymentOrder(ctx, who, closed.ID, strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired grace claim err=%v", err)
	}
}
