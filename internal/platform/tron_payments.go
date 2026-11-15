package platform

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/payments"
	"reladock/internal/tronpayments"
)

var tronTransactionHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

const tronPaymentClaimGrace = 24 * time.Hour

type CreateTronPaymentOrderInput struct {
	AmountCents    int64  `json:"amountCents"`
	RuleID         string `json:"ruleId,omitempty"`
	PriceVersionID string `json:"priceVersionId,omitempty"`
}

type TronPaymentOrder struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"-"`
	CreatedBy         string     `json:"-"`
	Purpose           string     `json:"purpose"`
	RuleID            string     `json:"ruleId,omitempty"`
	PriceVersionID    string     `json:"priceVersionId,omitempty"`
	AmountCents       int64      `json:"amountCents"`
	AmountBaseUnits   int64      `json:"amountBaseUnits"`
	Currency          string     `json:"currency"`
	Asset             string     `json:"asset"`
	Network           string     `json:"network"`
	CollectionAddress string     `json:"collectionAddress"`
	State             string     `json:"state"`
	TxHash            string     `json:"txHash,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	ServerTime        time.Time  `json:"serverTime"`
	ClaimedAt         *time.Time `json:"claimedAt,omitempty"`
	ClaimAllowed      bool       `json:"claimAllowed"`
	NextAttemptAt     *time.Time `json:"-"`
	Attempts          int        `json:"-"`
	LeaseToken        string     `json:"-"`
	LeaseExpiresAt    *time.Time `json:"-"`
	LastError         string     `json:"lastError,omitempty"`
	CreditLedgerID    int64      `json:"creditLedgerId,omitempty"`
	PurchaseState     string     `json:"purchaseState"`
	PurchaseError     string     `json:"purchaseError,omitempty"`
	SubscriptionID    string     `json:"subscriptionId,omitempty"`
}

const tronPaymentOrderColumns = `id::text,tenant_id::text,created_by::text,purpose,COALESCE(rule_id::text,''),COALESCE(price_version_id::text,''),amount_cents,amount_base_units,currency,asset,network,collection_address,state,COALESCE(tx_hash,''),created_at,expires_at,claimed_at,next_attempt_at,attempts,COALESCE(lease_token::text,''),lease_expires_at,last_error,COALESCE(credit_ledger_id,0),purchase_state,purchase_error,COALESCE(subscription_id::text,'')`

func scanTronPaymentOrder(row pgx.Row) (TronPaymentOrder, error) {
	var order TronPaymentOrder
	err := row.Scan(
		&order.ID, &order.TenantID, &order.CreatedBy, &order.Purpose, &order.RuleID,
		&order.PriceVersionID, &order.AmountCents, &order.AmountBaseUnits, &order.Currency,
		&order.Asset, &order.Network, &order.CollectionAddress, &order.State, &order.TxHash,
		&order.CreatedAt, &order.ExpiresAt, &order.ClaimedAt, &order.NextAttemptAt,
		&order.Attempts, &order.LeaseToken, &order.LeaseExpiresAt, &order.LastError,
		&order.CreditLedgerID, &order.PurchaseState, &order.PurchaseError, &order.SubscriptionID,
	)
	return order, err
}

func presentTronPaymentOrder(order TronPaymentOrder) TronPaymentOrder {
	now := time.Now().UTC()
	order.ServerTime = now
	if order.State == "pending" {
		order.ClaimAllowed = now.Before(order.ExpiresAt.Add(tronPaymentClaimGrace))
		if !now.Before(order.ExpiresAt) {
			order.State = "expired"
		}
	}
	return order
}

func (s *Store) CreateTronPaymentOrder(ctx context.Context, who Identity, key, collectionAddress string, input CreateTronPaymentOrderInput) (TronPaymentOrder, error) {
	if !billingUUID.MatchString(who.TenantID) || !billingUUID.MatchString(who.MemberID) || !idempotencyKey.MatchString(key) || tronpayments.ValidateMainnetAddress(collectionAddress) != nil {
		return TronPaymentOrder{}, ErrInvalid
	}
	planPurchase := input.PriceVersionID != "" || input.RuleID != ""
	if planPurchase {
		if input.AmountCents != 0 || !billingUUID.MatchString(input.PriceVersionID) || input.RuleID != "" && !billingUUID.MatchString(input.RuleID) {
			return TronPaymentOrder{}, ErrInvalid
		}
	} else if input.AmountCents < payments.MinTopUpCents || input.AmountCents > payments.MaxTopUpCents {
		return TronPaymentOrder{}, ErrInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
		return TronPaymentOrder{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "tron-order:"+who.TenantID+":"+key); err != nil {
		return TronPaymentOrder{}, err
	}

	purpose, purchaseState := "wallet-topup", "none"
	amount := input.AmountCents
	if planPurchase {
		purpose, purchaseState = "plan-purchase", "pending"
		if input.RuleID != "" {
			var owned string
			err = tx.QueryRow(ctx, "SELECT id::text FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'", input.RuleID, who.TenantID).Scan(&owned)
			if errors.Is(err, pgx.ErrNoRows) {
				return TronPaymentOrder{}, ErrNotFound
			}
			if err != nil {
				return TronPaymentOrder{}, err
			}
		}
		err = tx.QueryRow(ctx, "SELECT price_cents FROM plan_price_versions WHERE id=$1", input.PriceVersionID).Scan(&amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return TronPaymentOrder{}, ErrNotFound
		}
		if err != nil {
			return TronPaymentOrder{}, err
		}
		if amount < payments.MinTopUpCents || amount > payments.MaxTopUpCents {
			return TronPaymentOrder{}, ErrInvalid
		}
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "tron-quote:"+strconv.FormatInt(amount, 10)); err != nil {
		return TronPaymentOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE tron_payment_orders SET state='expired' WHERE amount_cents=$1 AND state='pending' AND expires_at+$2::interval<=clock_timestamp()`, amount, tronPaymentClaimGrace.String()); err != nil {
		return TronPaymentOrder{}, err
	}
	var baseUnits int64
	err = tx.QueryRow(ctx, `SELECT $1::bigint*10000+suffix FROM generate_series(0,9999) suffix WHERE NOT EXISTS (SELECT 1 FROM tron_payment_orders WHERE amount_base_units=$1::bigint*10000+suffix AND state IN ('pending','confirming')) ORDER BY suffix LIMIT 1`, amount).Scan(&baseUnits)
	if errors.Is(err, pgx.ErrNoRows) {
		return TronPaymentOrder{}, ErrCapacity
	}
	if err != nil {
		return TronPaymentOrder{}, err
	}
	result, err := tx.Exec(ctx, `INSERT INTO tron_payment_orders(id,tenant_id,created_by,idempotency_key,purpose,rule_id,price_version_id,amount_cents,amount_base_units,collection_address,purchase_state) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8,$9,$10,$11) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, newID(), who.TenantID, who.MemberID, key, purpose, input.RuleID, input.PriceVersionID, amount, baseUnits, collectionAddress, purchaseState)
	if err != nil {
		return TronPaymentOrder{}, memberConflict(err)
	}
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE tenant_id=$1 AND idempotency_key=$2", who.TenantID, key))
	if err != nil {
		return TronPaymentOrder{}, err
	}
	if order.Purpose != purpose || order.RuleID != input.RuleID || order.PriceVersionID != input.PriceVersionID || order.AmountCents != amount || order.AmountBaseUnits < amount*10000 || order.AmountBaseUnits >= (amount+1)*10000 || order.CollectionAddress != collectionAddress {
		return TronPaymentOrder{}, ErrConflict
	}
	if result.RowsAffected() == 1 {
		if err = platformLog(ctx, tx, &who, "tron-payment.order", order.ID, map[string]any{"tenantId": who.TenantID, "purpose": purpose, "amountCents": amount}); err != nil {
			return TronPaymentOrder{}, err
		}
	}
	return presentTronPaymentOrder(order), tx.Commit(ctx)
}

func (s *Store) ClaimTronPaymentOrder(ctx context.Context, who Identity, id, txHash string) (TronPaymentOrder, error) {
	txHash = strings.TrimSpace(txHash)
	if !billingUUID.MatchString(id) || !tronTransactionHash.MatchString(txHash) {
		return TronPaymentOrder{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
		return TronPaymentOrder{}, err
	}
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1 AND tenant_id=$2 FOR UPDATE", id, who.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TronPaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return TronPaymentOrder{}, err
	}
	if order.TxHash != "" {
		if order.TxHash != txHash {
			return TronPaymentOrder{}, ErrConflict
		}
		return presentTronPaymentOrder(order), tx.Commit(ctx)
	}
	if order.State != "pending" {
		return TronPaymentOrder{}, ErrConflict
	}
	if !time.Now().Before(order.ExpiresAt.Add(tronPaymentClaimGrace)) {
		if _, err = tx.Exec(ctx, "UPDATE tron_payment_orders SET state='expired' WHERE id=$1", id); err != nil {
			return TronPaymentOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return TronPaymentOrder{}, err
		}
		return TronPaymentOrder{}, ErrConflict
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `UPDATE tron_payment_orders SET tx_hash=$2,state='confirming',claimed_at=$3,next_attempt_at=clock_timestamp(),last_error='' WHERE id=$1`, id, txHash, now)
	if err != nil {
		return TronPaymentOrder{}, memberConflict(err)
	}
	if err = platformLog(ctx, tx, &who, "tron-payment.claim", order.ID, map[string]any{"tenantId": who.TenantID}); err != nil {
		return TronPaymentOrder{}, err
	}
	order.TxHash, order.State, order.ClaimedAt = txHash, "confirming", &now
	return presentTronPaymentOrder(order), tx.Commit(ctx)
}

func (s *Store) GetTronPaymentOrder(ctx context.Context, who Identity, id string) (TronPaymentOrder, error) {
	if !billingUUID.MatchString(id) {
		return TronPaymentOrder{}, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1 AND tenant_id=$2", id, who.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TronPaymentOrder{}, ErrNotFound
	}
	return presentTronPaymentOrder(order), err
}

func (s *Store) ListTronPaymentOrders(ctx context.Context, who Identity) ([]TronPaymentOrder, error) {
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE tenant_id=$1 ORDER BY created_at DESC,id DESC LIMIT 20", who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []TronPaymentOrder{}
	for rows.Next() {
		order, scanErr := scanTronPaymentOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, presentTronPaymentOrder(order))
	}
	return orders, rows.Err()
}

type TronScanLease struct {
	CursorID string
	Height   int64
	Token    string
}

func (s *Store) ClaimNextTronScanBlock(ctx context.Context, collectionAddress string, latestHeight, initialLookback int64, leaseDuration time.Duration) (*TronScanLease, error) {
	if tronpayments.ValidateMainnetAddress(collectionAddress) != nil || latestHeight <= 0 || initialLookback < 1 || initialLookback > 100000 || leaseDuration < time.Second || leaseDuration > 5*time.Minute {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "tron-scan:"+collectionAddress); err != nil {
		return nil, err
	}
	var cursorID string
	var nextBlock int64
	var nextAttempt time.Time
	var leaseToken string
	var leaseExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,next_block,next_attempt_at,COALESCE(lease_token::text,''),lease_expires_at FROM tron_payment_scan_cursors WHERE network='TRON' AND contract_address=$1 AND collection_address=$2 FOR UPDATE`, tronpayments.OfficialUSDTAddress, collectionAddress).Scan(&cursorID, &nextBlock, &nextAttempt, &leaseToken, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		cursorID = newID()
		nextBlock = latestHeight - initialLookback
		if nextBlock < 1 {
			nextBlock = 1
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tron_payment_scan_cursors(id,network,contract_address,collection_address,next_block) VALUES($1,'TRON',$2,$3,$4)`, cursorID, tronpayments.OfficialUSDTAddress, collectionAddress, nextBlock); err != nil {
			return nil, err
		}
		nextAttempt = time.Time{}
		leaseToken, leaseExpires = "", nil
	} else if err != nil {
		return nil, err
	}
	var databaseNow time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
		return nil, err
	}
	if nextBlock > latestHeight || nextAttempt.After(databaseNow) || leaseToken != "" && leaseExpires != nil && leaseExpires.After(databaseNow) {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	token := newID()
	result, err := tx.Exec(ctx, `UPDATE tron_payment_scan_cursors SET lease_token=$2,lease_expires_at=clock_timestamp()+$3::interval,updated_at=clock_timestamp() WHERE id=$1 AND next_block=$4`, cursorID, token, leaseDuration.String(), nextBlock)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &TronScanLease{CursorID: cursorID, Height: nextBlock, Token: token}, nil
}

func (s *Store) CompleteTronScanBlock(ctx context.Context, lease TronScanLease) error {
	if !billingUUID.MatchString(lease.CursorID) || !billingUUID.MatchString(lease.Token) || lease.Height <= 0 {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE tron_payment_scan_cursors SET next_block=next_block+1,next_attempt_at=clock_timestamp(),lease_token=NULL,lease_expires_at=NULL,last_error='',updated_at=clock_timestamp() WHERE id=$1 AND next_block=$2 AND lease_token=$3 AND lease_expires_at>clock_timestamp()`, lease.CursorID, lease.Height, lease.Token)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) RetryTronScanBlock(ctx context.Context, lease TronScanLease, reason string, delay time.Duration) error {
	if !billingUUID.MatchString(lease.CursorID) || !billingUUID.MatchString(lease.Token) || lease.Height <= 0 || reason != "provider-unavailable" || delay < time.Second || delay > 10*time.Minute {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE tron_payment_scan_cursors SET next_attempt_at=clock_timestamp()+$4::interval,lease_token=NULL,lease_expires_at=NULL,last_error=$5,updated_at=clock_timestamp() WHERE id=$1 AND next_block=$2 AND lease_token=$3 AND lease_expires_at>clock_timestamp()`, lease.CursorID, lease.Height, lease.Token, delay.String(), reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) MatchDiscoveredTronPayment(ctx context.Context, receipt tronpayments.Receipt) (*TronPaymentOrder, error) {
	receipt.TxHash = strings.ToLower(receipt.TxHash)
	if !tronTransactionHash.MatchString(receipt.TxHash) || receipt.BlockNumber <= 0 || receipt.LogIndex < 0 || receipt.BlockTime.IsZero() || receipt.AmountBaseUnits <= 0 || receipt.ContractAddress != tronpayments.OfficialUSDTAddress || tronpayments.ValidateMainnetAddress(receipt.FromAddress) != nil || tronpayments.ValidateMainnetAddress(receipt.ToAddress) != nil {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	findAttached := func() (TronPaymentOrder, error) {
		return scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+` FROM tron_payment_orders WHERE tx_hash=$1 AND amount_base_units=$2 AND collection_address=$3 AND created_at<=$4 AND expires_at>=$4 ORDER BY id LIMIT 1 FOR UPDATE`, receipt.TxHash, receipt.AmountBaseUnits, receipt.ToAddress, receipt.BlockTime))
	}
	attached, err := findAttached()
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &attached, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+` FROM tron_payment_orders WHERE state IN ('pending','expired') AND tx_hash IS NULL AND amount_base_units=$1 AND collection_address=$2 AND created_at<=$3 AND expires_at>=$3 ORDER BY created_at,id LIMIT 1 FOR UPDATE`, receipt.AmountBaseUnits, receipt.ToAddress, receipt.BlockTime))
	if errors.Is(err, pgx.ErrNoRows) {
		attached, attachedErr := findAttached()
		if attachedErr == nil {
			if err = tx.Commit(ctx); err != nil {
				return nil, err
			}
			return &attached, nil
		}
		if !errors.Is(attachedErr, pgx.ErrNoRows) {
			return nil, attachedErr
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var claimedAt time.Time
	result, err := tx.Exec(ctx, `UPDATE tron_payment_orders SET tx_hash=$2,state='confirming',claimed_at=clock_timestamp(),next_attempt_at=clock_timestamp(),last_error='' WHERE id=$1 AND state IN ('pending','expired') AND tx_hash IS NULL`, order.ID, receipt.TxHash)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, ErrConflict
	}
	if err = tx.QueryRow(ctx, `SELECT claimed_at FROM tron_payment_orders WHERE id=$1`, order.ID).Scan(&claimedAt); err != nil {
		return nil, err
	}
	if err = tronPaymentAudit(ctx, tx, "tron-payment.auto-claim", order.ID); err != nil {
		return nil, err
	}
	order.TxHash, order.State, order.ClaimedAt = receipt.TxHash, "confirming", &claimedAt
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &order, nil
}

func (s *Store) ClaimPendingTronPaymentConfirmation(ctx context.Context, leaseDuration time.Duration) (*TronPaymentOrder, error) {
	if leaseDuration < time.Second || leaseDuration > 5*time.Minute {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+` FROM tron_payment_orders WHERE (state='confirming' OR (state='credited' AND purpose='plan-purchase' AND purchase_state='pending')) AND next_attempt_at<=clock_timestamp() AND (lease_expires_at IS NULL OR lease_expires_at<=clock_timestamp()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token := newID()
	var leaseExpires time.Time
	if err = tx.QueryRow(ctx, "UPDATE tron_payment_orders SET lease_token=$2,lease_expires_at=clock_timestamp()+$3::interval,attempts=attempts+1 WHERE id=$1 RETURNING lease_expires_at", order.ID, token, leaseDuration.String()).Scan(&leaseExpires); err != nil {
		return nil, err
	}
	order.LeaseToken, order.LeaseExpiresAt, order.Attempts = token, &leaseExpires, order.Attempts+1
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &order, nil
}

func (s *Store) ApplyTronPaymentReceipt(ctx context.Context, orderID, leaseToken string, receipt tronpayments.Receipt) (TronPaymentOrder, error) {
	if !billingUUID.MatchString(orderID) || !billingUUID.MatchString(leaseToken) || !tronTransactionHash.MatchString(receipt.TxHash) {
		return TronPaymentOrder{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1 FOR UPDATE", orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TronPaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return TronPaymentOrder{}, err
	}
	if order.State == "credited" {
		matches, matchErr := tronReceiptRecorded(ctx, tx, order, receipt)
		if matchErr != nil {
			return TronPaymentOrder{}, matchErr
		}
		if !matches {
			return TronPaymentOrder{}, ErrConflict
		}
		return order, tx.Commit(ctx)
	}
	var leaseCurrent bool
	if order.LeaseExpiresAt != nil {
		err = tx.QueryRow(ctx, "SELECT lease_expires_at>clock_timestamp() FROM tron_payment_orders WHERE id=$1", order.ID).Scan(&leaseCurrent)
		if err != nil {
			return TronPaymentOrder{}, err
		}
	}
	if order.State != "confirming" || order.LeaseToken != leaseToken || !leaseCurrent {
		return TronPaymentOrder{}, ErrConflict
	}
	if !validTronReceiptForOrder(order, receipt) {
		return tronPaymentReview(ctx, tx, order, "receipt-mismatch")
	}
	var receiptOwner string
	err = tx.QueryRow(ctx, "SELECT order_id::text FROM tron_payment_receipts WHERE tx_hash=$1", receipt.TxHash).Scan(&receiptOwner)
	if err == nil && receiptOwner != order.ID {
		return tronPaymentReview(ctx, tx, order, "transaction-already-used")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return TronPaymentOrder{}, err
	}

	if _, err = tx.Exec(ctx, `INSERT INTO tron_payment_receipts(tx_hash,order_id,tenant_id,log_index,from_address,to_address,contract_address,amount_base_units,block_number,block_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, receipt.TxHash, order.ID, order.TenantID, receipt.LogIndex, receipt.FromAddress, receipt.ToAddress, receipt.ContractAddress, receipt.AmountBaseUnits, receipt.BlockNumber, receipt.BlockTime); err != nil {
		return TronPaymentOrder{}, memberConflict(err)
	}
	balance, err := paymentWalletLock(ctx, tx, order.TenantID)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	if balance > math.MaxInt64-order.AmountCents {
		return tronPaymentReview(ctx, tx, order, "wallet-overflow")
	}
	balance += order.AmountCents
	if _, err = tx.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", order.TenantID, balance); err != nil {
		return TronPaymentOrder{}, err
	}
	var ledgerID int64
	err = tx.QueryRow(ctx, `INSERT INTO billing_ledger(tenant_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name) VALUES($1,'payment-credit',$2,$3,$4,'Verified solidified TRON USDT transfer','tron-usdt-worker') RETURNING id`, order.TenantID, order.AmountCents, balance, "tron-credit:"+order.ID).Scan(&ledgerID)
	if err != nil {
		return TronPaymentOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE tron_payment_orders SET state='credited',credit_ledger_id=$2,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=CASE WHEN purpose='plan-purchase' THEN clock_timestamp() ELSE NULL END,last_error='' WHERE id=$1`, order.ID, ledgerID); err != nil {
		return TronPaymentOrder{}, err
	}
	if err = tronPaymentAudit(ctx, tx, "tron-payment.credit", order.ID); err != nil {
		return TronPaymentOrder{}, err
	}
	order.State, order.CreditLedgerID = "credited", ledgerID
	order.LeaseToken, order.LeaseExpiresAt, order.NextAttemptAt, order.LastError = "", nil, nil, ""
	return order, tx.Commit(ctx)
}

func validTronReceiptForOrder(order TronPaymentOrder, receipt tronpayments.Receipt) bool {
	return receipt.TxHash == order.TxHash && receipt.BlockNumber > 0 && receipt.LogIndex >= 0 &&
		receipt.AmountBaseUnits == order.AmountBaseUnits && receipt.ToAddress == order.CollectionAddress &&
		receipt.ContractAddress == tronpayments.OfficialUSDTAddress &&
		tronpayments.ValidateMainnetAddress(receipt.FromAddress) == nil &&
		tronpayments.ValidateMainnetAddress(receipt.ToAddress) == nil &&
		!receipt.BlockTime.Before(order.CreatedAt) && !receipt.BlockTime.After(order.ExpiresAt)
}

func tronReceiptRecorded(ctx context.Context, tx pgx.Tx, order TronPaymentOrder, receipt tronpayments.Receipt) (bool, error) {
	var stored tronpayments.Receipt
	err := tx.QueryRow(ctx, `SELECT tx_hash,log_index,from_address,to_address,contract_address,amount_base_units,block_number,block_time FROM tron_payment_receipts WHERE order_id=$1`, order.ID).Scan(&stored.TxHash, &stored.LogIndex, &stored.FromAddress, &stored.ToAddress, &stored.ContractAddress, &stored.AmountBaseUnits, &stored.BlockNumber, &stored.BlockTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stored.TxHash == receipt.TxHash && stored.LogIndex == receipt.LogIndex && stored.FromAddress == receipt.FromAddress && stored.ToAddress == receipt.ToAddress && stored.ContractAddress == receipt.ContractAddress && stored.AmountBaseUnits == receipt.AmountBaseUnits && stored.BlockNumber == receipt.BlockNumber && stored.BlockTime.Equal(receipt.BlockTime), nil
}

func tronPaymentReview(ctx context.Context, tx pgx.Tx, order TronPaymentOrder, reason string) (TronPaymentOrder, error) {
	if _, err := tx.Exec(ctx, `UPDATE tron_payment_orders SET state='needs-review',last_error=$2,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=NULL WHERE id=$1`, order.ID, reason); err != nil {
		return TronPaymentOrder{}, err
	}
	if err := tronPaymentAudit(ctx, tx, "tron-payment.needs-review", order.ID); err != nil {
		return TronPaymentOrder{}, err
	}
	order.State, order.LastError = "needs-review", reason
	order.LeaseToken, order.LeaseExpiresAt, order.NextAttemptAt = "", nil, nil
	if err := tx.Commit(ctx); err != nil {
		return TronPaymentOrder{}, err
	}
	return order, nil
}

func tronPaymentAudit(ctx context.Context, tx pgx.Tx, action, orderID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform_audit_logs(actor_email,action,resource_id,details) VALUES('tron-usdt-worker',$1,$2,jsonb_build_object('provider','tron-usdt'))`, action, orderID)
	return err
}

func (s *Store) RetryTronPaymentConfirmation(ctx context.Context, orderID, leaseToken, reason string, delay time.Duration) error {
	if !billingUUID.MatchString(orderID) || !billingUUID.MatchString(leaseToken) || (reason != "not-solidified" && reason != "provider-unavailable") || delay < time.Second || delay > 10*time.Minute {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE tron_payment_orders SET next_attempt_at=clock_timestamp()+$3::interval,lease_token=NULL,lease_expires_at=NULL,last_error=$4 WHERE id=$1 AND state='confirming' AND lease_token=$2 AND lease_expires_at>clock_timestamp()`, orderID, leaseToken, delay.String(), reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) ReviewTronPaymentConfirmation(ctx context.Context, orderID, leaseToken, reason string) error {
	if !billingUUID.MatchString(orderID) || !billingUUID.MatchString(leaseToken) || (reason != "invalid-receipt" && reason != "confirmation-timeout") {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	order, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1 FOR UPDATE", orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var leaseCurrent bool
	if order.LeaseExpiresAt != nil {
		if err = tx.QueryRow(ctx, "SELECT lease_expires_at>clock_timestamp() FROM tron_payment_orders WHERE id=$1", order.ID).Scan(&leaseCurrent); err != nil {
			return err
		}
	}
	if order.State != "confirming" || order.LeaseToken != leaseToken || !leaseCurrent {
		return ErrConflict
	}
	_, err = tronPaymentReview(ctx, tx, order, reason)
	return err
}

// CompleteTronPlanPurchase runs after the independently committed wallet credit.
// A business refusal is persisted on the order and never rolls the credit back.
func (s *Store) CompleteTronPlanPurchase(ctx context.Context, orderID string) error {
	if !billingUUID.MatchString(orderID) {
		return ErrInvalid
	}
	order, err := scanTronPaymentOrder(s.pool.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1", orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if order.Purpose != "plan-purchase" || order.State != "credited" || order.PurchaseState == "purchased" || order.PurchaseState == "failed" {
		return nil
	}
	var who Identity
	err = s.pool.QueryRow(ctx, `SELECT m.id::text,m.tenant_id::text,m.email,m.role,t.name FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 AND t.status='active'`, order.CreatedBy, order.TenantID).Scan(&who.MemberID, &who.TenantID, &who.Email, &who.Role, &who.Company)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.finishTronPlanPurchase(ctx, order, RuleSubscription{}, "unauthorized")
	}
	if err != nil {
		return err
	}
	if !canWrite(who) {
		return s.finishTronPlanPurchase(ctx, order, RuleSubscription{}, "unauthorized")
	}
	subscription, err := s.PurchasePlan(ctx, who, "tron-purchase:"+order.ID, PurchasePlanInput{RuleID: order.RuleID, PriceVersionID: order.PriceVersionID})
	if err != nil {
		reason, business := planPaymentBusinessError(err)
		if errors.Is(err, ErrUnauthorized) {
			reason, business = "unauthorized", true
		}
		if !business {
			return err
		}
		return s.finishTronPlanPurchase(ctx, order, RuleSubscription{}, reason)
	}
	return s.finishTronPlanPurchase(ctx, order, subscription, "")
}

func (s *Store) finishTronPlanPurchase(ctx context.Context, order TronPaymentOrder, subscription RuleSubscription, failure string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := scanTronPaymentOrder(tx.QueryRow(ctx, "SELECT "+tronPaymentOrderColumns+" FROM tron_payment_orders WHERE id=$1 FOR UPDATE", order.ID))
	if err != nil {
		return err
	}
	if current.State != "credited" || current.PurchaseState != "pending" {
		return tx.Commit(ctx)
	}
	if failure != "" {
		if len(failure) > 120 {
			failure = "unavailable"
		}
		if _, err = tx.Exec(ctx, `UPDATE tron_payment_orders SET purchase_state='failed',purchase_error=$2,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=NULL WHERE id=$1`, order.ID, failure); err != nil {
			return err
		}
		if err = tronPaymentAudit(ctx, tx, "tron-payment.plan-failed", order.ID); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE tron_payment_orders SET purchase_state='purchased',purchase_error='',subscription_id=$2,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=NULL WHERE id=$1`, order.ID, subscription.ID); err != nil {
			return err
		}
		if err = tronPaymentAudit(ctx, tx, "tron-payment.plan-purchased", order.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
