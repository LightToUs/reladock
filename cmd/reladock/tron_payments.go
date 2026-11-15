package main

import (
	"context"
	"errors"
	"log"
	"time"

	"reladock/internal/platform"
	"reladock/internal/tronpayments"
)

type tronPaymentStore interface {
	ClaimPendingTronPaymentConfirmation(context.Context, time.Duration) (*platform.TronPaymentOrder, error)
	ApplyTronPaymentReceipt(context.Context, string, string, tronpayments.Receipt) (platform.TronPaymentOrder, error)
	RetryTronPaymentConfirmation(context.Context, string, string, string, time.Duration) error
	ReviewTronPaymentConfirmation(context.Context, string, string, string) error
	CompleteTronPlanPurchase(context.Context, string) error
}

type tronReceiptVerifier interface {
	Verify(context.Context, string, int64) (tronpayments.Receipt, error)
}

type tronScanStore interface {
	ClaimNextTronScanBlock(context.Context, string, int64, int64, time.Duration) (*platform.TronScanLease, error)
	CompleteTronScanBlock(context.Context, platform.TronScanLease) error
	RetryTronScanBlock(context.Context, platform.TronScanLease, string, time.Duration) error
	MatchDiscoveredTronPayment(context.Context, tronpayments.Receipt) (*platform.TronPaymentOrder, error)
}

type tronSolidBlockScanner interface {
	LatestSolidBlock(context.Context) (int64, error)
	ScanSolidBlock(context.Context, int64) ([]tronpayments.Receipt, error)
}

const (
	tronConfirmationMaxAttempts = 40
	tronConfirmationHorizon     = 30 * time.Minute
	tronScanInitialLookback     = int64(1200)
	tronScanBatchSize           = 20
)

func processTronPayment(ctx context.Context, store tronPaymentStore, verifier tronReceiptVerifier) error {
	order, err := store.ClaimPendingTronPaymentConfirmation(ctx, time.Minute)
	if err != nil || order == nil {
		return err
	}
	if order.State == "credited" && order.Purpose == "plan-purchase" && order.PurchaseState == "pending" {
		return store.CompleteTronPlanPurchase(ctx, order.ID)
	}
	if order.Attempts >= tronConfirmationMaxAttempts || order.ClaimedAt != nil && time.Since(*order.ClaimedAt) > tronConfirmationHorizon {
		return store.ReviewTronPaymentConfirmation(ctx, order.ID, order.LeaseToken, "confirmation-timeout")
	}
	receipt, err := verifier.Verify(ctx, order.TxHash, order.AmountBaseUnits)
	switch {
	case errors.Is(err, tronpayments.ErrPending):
		return store.RetryTronPaymentConfirmation(ctx, order.ID, order.LeaseToken, "not-solidified", 15*time.Second)
	case errors.Is(err, tronpayments.ErrProvider):
		return store.RetryTronPaymentConfirmation(ctx, order.ID, order.LeaseToken, "provider-unavailable", time.Minute)
	case errors.Is(err, tronpayments.ErrInvalidReceipt):
		return store.ReviewTronPaymentConfirmation(ctx, order.ID, order.LeaseToken, "invalid-receipt")
	case err != nil:
		return err
	}
	credited, err := store.ApplyTronPaymentReceipt(ctx, order.ID, order.LeaseToken, receipt)
	if err != nil {
		return err
	}
	if credited.Purpose == "plan-purchase" {
		return store.CompleteTronPlanPurchase(ctx, credited.ID)
	}
	return nil
}

func processTronScan(ctx context.Context, store tronScanStore, scanner tronSolidBlockScanner, collectionAddress string, maxBlocks int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if maxBlocks < 1 || maxBlocks > 100 {
		return errors.New("invalid TRON scan batch size")
	}
	latest, err := scanner.LatestSolidBlock(ctx)
	if err != nil {
		return err
	}
	for range maxBlocks {
		if err = ctx.Err(); err != nil {
			return err
		}
		lease, claimErr := store.ClaimNextTronScanBlock(ctx, collectionAddress, latest, tronScanInitialLookback, time.Minute)
		if claimErr != nil {
			return claimErr
		}
		if lease == nil {
			return nil
		}
		receipts, scanErr := scanner.ScanSolidBlock(ctx, lease.Height)
		if errors.Is(scanErr, tronpayments.ErrProvider) {
			return store.RetryTronScanBlock(ctx, *lease, "provider-unavailable", time.Minute)
		}
		if scanErr != nil {
			return scanErr
		}
		for _, receipt := range receipts {
			if _, matchErr := store.MatchDiscoveredTronPayment(ctx, receipt); matchErr != nil {
				return matchErr
			}
		}
		if err = store.CompleteTronScanBlock(ctx, *lease); err != nil {
			return err
		}
	}
	return nil
}

func processTronPaymentCycle(ctx context.Context, store interface {
	tronPaymentStore
	tronScanStore
}, verifier tronReceiptVerifier, scanner tronSolidBlockScanner, collectionAddress string) error {
	if err := processTronPayment(ctx, store, verifier); err != nil {
		return err
	}
	return processTronScan(ctx, store, scanner, collectionAddress, tronScanBatchSize)
}

func runTronPayments(ctx context.Context, store interface {
	tronPaymentStore
	tronScanStore
}, verifier tronReceiptVerifier, scanner tronSolidBlockScanner, collectionAddress string, observeConfirm, observeScan func(error)) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		workerCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		confirmErr := processTronPayment(workerCtx, store, verifier)
		if observeConfirm != nil {
			observeConfirm(confirmErr)
		}
		var scanErr error
		if confirmErr == nil {
			scanErr = processTronScan(workerCtx, store, scanner, collectionAddress, tronScanBatchSize)
		}
		cancel()
		if observeScan != nil {
			observeScan(scanErr)
		}
		if confirmErr != nil && ctx.Err() == nil {
			log.Print("TRON payment confirmation worker failed")
		}
		if scanErr != nil && ctx.Err() == nil {
			log.Print("TRON payment scan worker failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
