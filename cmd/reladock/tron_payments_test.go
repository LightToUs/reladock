package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"reladock/internal/platform"
	"reladock/internal/tronpayments"
)

const testTronWorkerCollection = "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw"

type fakeTronWorkerStore struct {
	order       *platform.TronPaymentOrder
	applied     bool
	retried     string
	reviewed    string
	planHandled bool
	scanLeases  []platform.TronScanLease
	scanClaims  int
	scanDone    []int64
	scanRetried []int64
	discovered  []tronpayments.Receipt
	sequence    []string
}

func (f *fakeTronWorkerStore) ClaimPendingTronPaymentConfirmation(context.Context, time.Duration) (*platform.TronPaymentOrder, error) {
	f.sequence = append(f.sequence, "confirm")
	return f.order, nil
}
func (f *fakeTronWorkerStore) ApplyTronPaymentReceipt(_ context.Context, _ string, _ string, _ tronpayments.Receipt) (platform.TronPaymentOrder, error) {
	f.applied = true
	result := *f.order
	result.State = "credited"
	return result, nil
}
func (f *fakeTronWorkerStore) RetryTronPaymentConfirmation(_ context.Context, _, _, reason string, _ time.Duration) error {
	f.retried = reason
	return nil
}
func (f *fakeTronWorkerStore) ReviewTronPaymentConfirmation(_ context.Context, _, _, reason string) error {
	f.reviewed = reason
	return nil
}
func (f *fakeTronWorkerStore) CompleteTronPlanPurchase(context.Context, string) error {
	f.planHandled = true
	return nil
}
func (f *fakeTronWorkerStore) ClaimNextTronScanBlock(_ context.Context, collection string, _ int64, lookback int64, _ time.Duration) (*platform.TronScanLease, error) {
	if collection != testTronWorkerCollection || lookback != tronScanInitialLookback {
		return nil, errors.New("wrong scan identity")
	}
	f.sequence = append(f.sequence, "scan")
	if f.scanClaims >= len(f.scanLeases) {
		return nil, nil
	}
	lease := f.scanLeases[f.scanClaims]
	f.scanClaims++
	return &lease, nil
}
func (f *fakeTronWorkerStore) CompleteTronScanBlock(_ context.Context, lease platform.TronScanLease) error {
	f.scanDone = append(f.scanDone, lease.Height)
	return nil
}
func (f *fakeTronWorkerStore) RetryTronScanBlock(_ context.Context, lease platform.TronScanLease, reason string, _ time.Duration) error {
	if reason != "provider-unavailable" {
		return errors.New("wrong retry reason")
	}
	f.scanRetried = append(f.scanRetried, lease.Height)
	return nil
}
func (f *fakeTronWorkerStore) MatchDiscoveredTronPayment(_ context.Context, receipt tronpayments.Receipt) (*platform.TronPaymentOrder, error) {
	f.discovered = append(f.discovered, receipt)
	return nil, nil
}

type fakeTronVerifier struct {
	receipt tronpayments.Receipt
	err     error
}

func (f fakeTronVerifier) Verify(context.Context, string, int64) (tronpayments.Receipt, error) {
	return f.receipt, f.err
}

type fakeTronScanner struct {
	latest  int64
	blocks  map[int64][]tronpayments.Receipt
	errors  map[int64]error
	scanned []int64
}

func (f *fakeTronScanner) LatestSolidBlock(context.Context) (int64, error) {
	return f.latest, nil
}

func (f *fakeTronScanner) ScanSolidBlock(ctx context.Context, height int64) ([]tronpayments.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.scanned = append(f.scanned, height)
	if err := f.errors[height]; err != nil {
		return nil, err
	}
	return f.blocks[height], nil
}

func workerOrder() *platform.TronPaymentOrder {
	return &platform.TronPaymentOrder{ID: "00000000-0000-0000-0000-000000000001", State: "confirming", TxHash: strings.Repeat("a", 64), AmountBaseUnits: 5000000, LeaseToken: "00000000-0000-0000-0000-000000000002"}
}

func TestTronPaymentWorkerRetriesPendingAndProviderErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"pending":  {err: tronpayments.ErrPending, reason: "not-solidified"},
		"provider": {err: tronpayments.ErrProvider, reason: "provider-unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeTronWorkerStore{order: workerOrder()}
			if err := processTronPayment(context.Background(), store, fakeTronVerifier{err: tc.err}); err != nil {
				t.Fatal(err)
			}
			if store.retried != tc.reason || store.applied || store.reviewed != "" {
				t.Fatalf("store=%+v", store)
			}
		})
	}
}

func TestTronPaymentWorkerAppliesReceiptAndAttemptsPlan(t *testing.T) {
	store := &fakeTronWorkerStore{order: workerOrder()}
	store.order.Purpose = "plan-purchase"
	receipt := tronpayments.Receipt{TxHash: store.order.TxHash, AmountBaseUnits: store.order.AmountBaseUnits}
	if err := processTronPayment(context.Background(), store, fakeTronVerifier{receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	if !store.applied || !store.planHandled || store.retried != "" || store.reviewed != "" {
		t.Fatalf("store=%+v", store)
	}
}

func TestTronPaymentWorkerSendsInvalidReceiptToReview(t *testing.T) {
	store := &fakeTronWorkerStore{order: workerOrder()}
	if err := processTronPayment(context.Background(), store, fakeTronVerifier{err: tronpayments.ErrInvalidReceipt}); err != nil {
		t.Fatal(err)
	}
	if store.reviewed != "invalid-receipt" || store.applied || store.retried != "" {
		t.Fatalf("store=%+v", store)
	}
	store = &fakeTronWorkerStore{order: workerOrder()}
	if err := processTronPayment(context.Background(), store, fakeTronVerifier{err: errors.New("unexpected")}); err == nil {
		t.Fatal("unexpected verifier error hidden")
	}
}

func TestTronPaymentWorkerStopsUnboundedPendingRetries(t *testing.T) {
	store := &fakeTronWorkerStore{order: workerOrder()}
	store.order.Attempts = tronConfirmationMaxAttempts
	if err := processTronPayment(context.Background(), store, fakeTronVerifier{err: tronpayments.ErrPending}); err != nil {
		t.Fatal(err)
	}
	if store.reviewed != "confirmation-timeout" || store.retried != "" || store.applied {
		t.Fatalf("store=%+v", store)
	}
}

func scanLease(height int64) platform.TronScanLease {
	return platform.TronScanLease{CursorID: "00000000-0000-0000-0000-000000000010", Height: height, Token: "00000000-0000-0000-0000-000000000011"}
}

func TestTronScanWorkerMatchesCandidatesAndCompletesEmptyBlocks(t *testing.T) {
	receipt := tronpayments.Receipt{TxHash: strings.Repeat("b", 64), BlockNumber: 100, AmountBaseUnits: 1000000}
	store := &fakeTronWorkerStore{scanLeases: []platform.TronScanLease{scanLease(100), scanLease(101)}}
	scanner := &fakeTronScanner{latest: 101, blocks: map[int64][]tronpayments.Receipt{100: {receipt}}, errors: map[int64]error{}}
	if err := processTronScan(context.Background(), store, scanner, testTronWorkerCollection, 20); err != nil {
		t.Fatal(err)
	}
	if len(store.discovered) != 1 || store.discovered[0].TxHash != receipt.TxHash || len(store.scanDone) != 2 || store.scanDone[0] != 100 || store.scanDone[1] != 101 {
		t.Fatalf("store=%+v", store)
	}
}

func TestTronScanWorkerRetriesProviderWithoutAdvancingCursor(t *testing.T) {
	store := &fakeTronWorkerStore{scanLeases: []platform.TronScanLease{scanLease(200)}}
	scanner := &fakeTronScanner{latest: 200, blocks: map[int64][]tronpayments.Receipt{}, errors: map[int64]error{200: tronpayments.ErrProvider}}
	if err := processTronScan(context.Background(), store, scanner, testTronWorkerCollection, 20); err != nil {
		t.Fatal(err)
	}
	if len(store.scanRetried) != 1 || store.scanRetried[0] != 200 || len(store.scanDone) != 0 {
		t.Fatalf("store=%+v", store)
	}
}

func TestTronScanWorkerBoundsCatchupBatch(t *testing.T) {
	leasing := make([]platform.TronScanLease, 25)
	for i := range leasing {
		leasing[i] = scanLease(int64(300 + i))
	}
	store := &fakeTronWorkerStore{scanLeases: leasing}
	scanner := &fakeTronScanner{latest: 400, blocks: map[int64][]tronpayments.Receipt{}, errors: map[int64]error{}}
	if err := processTronScan(context.Background(), store, scanner, testTronWorkerCollection, 20); err != nil {
		t.Fatal(err)
	}
	if len(store.scanDone) != 20 || len(scanner.scanned) != 20 {
		t.Fatalf("done=%d scanned=%d", len(store.scanDone), len(scanner.scanned))
	}
}

func TestTronWorkerCyclePrioritizesConfirmationAndHonorsCancellation(t *testing.T) {
	store := &fakeTronWorkerStore{scanLeases: []platform.TronScanLease{scanLease(500)}}
	scanner := &fakeTronScanner{latest: 500, blocks: map[int64][]tronpayments.Receipt{}, errors: map[int64]error{}}
	if err := processTronPaymentCycle(context.Background(), store, fakeTronVerifier{}, scanner, testTronWorkerCollection); err != nil {
		t.Fatal(err)
	}
	if len(store.sequence) < 2 || store.sequence[0] != "confirm" || store.sequence[1] != "scan" {
		t.Fatalf("sequence=%v", store.sequence)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := processTronScan(canceled, store, scanner, testTronWorkerCollection, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err=%v", err)
	}
}
