package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"reladock/internal/platform"
	"reladock/internal/tronpayments"
)

type TronPayments struct {
	store             *platform.Store
	collectionAddress string
}

func NewTronPayments(store *platform.Store, collectionAddress string) *TronPayments {
	return &TronPayments{store: store, collectionAddress: collectionAddress}
}

func (p *TronPayments) Enabled() bool {
	return p != nil && p.store != nil && tronpayments.ValidateMainnetAddress(p.collectionAddress) == nil
}

func tronQRPayload(order platform.TronPaymentOrder) (string, error) {
	if order.State != "pending" || !time.Now().UTC().Before(order.ExpiresAt) {
		return "", platform.ErrConflict
	}
	if err := tronpayments.ValidateMainnetAddress(order.CollectionAddress); err != nil {
		return "", platform.ErrInvalid
	}
	return order.CollectionAddress, nil
}

func tronQRPNG(order platform.TronPaymentOrder) ([]byte, error) {
	payload, err := tronQRPayload(order)
	if err != nil {
		return nil, err
	}
	return qrcode.Encode(payload, qrcode.Medium, 256)
}

func (p *TronPayments) Serve(w http.ResponseWriter, r *http.Request, who platform.Identity) {
	if !p.Enabled() {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "TRON USDT 支付连接器未配置"})
		return
	}
	if len(r.URL.Query()) != 0 {
		failure(w, platform.ErrInvalid)
		return
	}
	const root = "/api/payments/tron/orders"
	if r.URL.Path == root {
		switch r.Method {
		case http.MethodGet:
			orders, err := p.store.ListTronPaymentOrders(r.Context(), who)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusOK, orders)
		case http.MethodPost:
			var input platform.CreateTronPaymentOrderInput
			if !decode(w, r, &input) {
				return
			}
			order, err := p.store.CreateTronPaymentOrder(r.Context(), who, r.Header.Get("Idempotency-Key"), p.collectionAddress, input)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusCreated, order)
		default:
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不支持"})
		}
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, root+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 1 && uuid.MatchString(parts[0]) && r.Method == http.MethodGet {
		order, err := p.store.GetTronPaymentOrder(r.Context(), who, parts[0])
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusOK, order)
		return
	}
	if len(parts) == 2 && uuid.MatchString(parts[0]) && parts[1] == "qr" && r.Method == http.MethodGet {
		order, err := p.store.GetTronPaymentOrder(r.Context(), who, parts[0])
		if err != nil {
			failure(w, err)
			return
		}
		png, err := tronQRPNG(order)
		if err != nil {
			if errors.Is(err, platform.ErrConflict) {
				failure(w, platform.ErrConflict)
				return
			}
			failure(w, platform.ErrInvalid)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png)
		return
	}
	if len(parts) == 2 && uuid.MatchString(parts[0]) && parts[1] == "claim" && r.Method == http.MethodPost {
		var input struct {
			TxHash string `json:"txHash"`
		}
		if !decode(w, r, &input) {
			return
		}
		order, err := p.store.ClaimTronPaymentOrder(r.Context(), who, parts[0], input.TxHash)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusOK, order)
		return
	}
	respond(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
}
