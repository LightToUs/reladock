package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"reladock/internal/platform"
)

func newTronRequest(method, path, body string, cookie *http.Cookie, key string) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "RelaDock")
	req.Header.Set("Origin", "http://127.0.0.1:3001")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestTronPaymentsHTTPCreateClaimListAndStatus(t *testing.T) {
	store := testWebStore(t)
	tron := NewTronPayments(store, "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw")
	handler := NewWithOptions(store, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{TronPayments: tron})
	registered := request(handler, http.MethodPost, "/api/auth/register", `{"company":"TRON","email":"tron-http@example.com","password":"long-test-password-42"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	cookie := registered.Result().Cookies()[0]
	status := request(handler, http.MethodGet, "/api/payments/status", "", cookie)
	if status.Code != http.StatusOK || status.Body.String() != "{\"enabled\":true,\"stripe\":false,\"tron\":true}\n" {
		t.Fatalf("status: %d %s", status.Code, status.Body.String())
	}

	create := serve(handler, newTronRequest(http.MethodPost, "/api/payments/tron/orders", `{"amountCents":500}`, cookie, "tron-http-order"))
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var order platform.TronPaymentOrder
	if err := json.Unmarshal(create.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.AmountCents != 500 || order.AmountBaseUnits != 5000000 || order.CollectionAddress == "" {
		t.Fatalf("order=%+v", order)
	}
	if order.ServerTime.IsZero() || order.ExpiresAt.Sub(order.ServerTime) < 29*time.Minute || order.ExpiresAt.Sub(order.ServerTime) > 31*time.Minute {
		t.Fatalf("server timing=%+v", order)
	}
	claim := serve(handler, newTronRequest(http.MethodPost, "/api/payments/tron/orders/"+order.ID+"/claim", `{"txHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, cookie, ""))
	if claim.Code != http.StatusOK || !json.Valid(claim.Body.Bytes()) {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	var claimed platform.TronPaymentOrder
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil || claimed.ServerTime.IsZero() {
		t.Fatalf("claim server timing: order=%+v err=%v", claimed, err)
	}
	list := request(handler, http.MethodGet, "/api/payments/tron/orders", "", cookie)
	if list.Code != http.StatusOK || !json.Valid(list.Body.Bytes()) {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
}

func TestTronPaymentsHTTPQRIsLocalTenantScopedAndUnavailableAfterExpiry(t *testing.T) {
	store := testWebStore(t)
	collection := "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw"
	tron := NewTronPayments(store, collection)
	handler := NewWithOptions(store, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{TronPayments: tron})
	first := request(handler, http.MethodPost, "/api/auth/register", `{"company":"QR A","email":"tron-qr-a@example.com","password":"long-test-password-42"}`, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("register first: %d %s", first.Code, first.Body.String())
	}
	firstCookie := first.Result().Cookies()[0]
	create := serve(handler, newTronRequest(http.MethodPost, "/api/payments/tron/orders", `{"amountCents":500}`, firstCookie, "tron-qr-order"))
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var order platform.TronPaymentOrder
	if err := json.Unmarshal(create.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	payload, err := tronQRPayload(order)
	if err != nil || payload != collection {
		t.Fatalf("payload=%q err=%v", payload, err)
	}
	qr := serve(handler, newTronRequest(http.MethodGet, "/api/payments/tron/orders/"+order.ID+"/qr", "", firstCookie, ""))
	if qr.Code != http.StatusOK || qr.Header().Get("Content-Type") != "image/png" || qr.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(qr.Body.String(), "\x89PNG\r\n\x1a\n") || qr.Body.Len() < 200 {
		t.Fatalf("qr: %d type=%q cache=%q bytes=%d", qr.Code, qr.Header().Get("Content-Type"), qr.Header().Get("Cache-Control"), qr.Body.Len())
	}
	second := request(handler, http.MethodPost, "/api/auth/register", `{"company":"QR B","email":"tron-qr-b@example.com","password":"long-test-password-42"}`, nil)
	secondCookie := second.Result().Cookies()[0]
	foreign := serve(handler, newTronRequest(http.MethodGet, "/api/payments/tron/orders/"+order.ID+"/qr", "", secondCookie, ""))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign qr: %d %s", foreign.Code, foreign.Body.String())
	}
	expiredOrder := order
	expiredOrder.State = "expired"
	if _, err = tronQRPayload(expiredOrder); err == nil {
		t.Fatal("expired order produced QR payload")
	}
	missing := serve(handler, newTronRequest(http.MethodGet, "/api/payments/tron/orders/00000000-0000-0000-0000-000000000099/qr", "", firstCookie, ""))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing qr: %d %s", missing.Code, missing.Body.String())
	}
}

func TestTronPaymentsHTTPDisabledAndStrictInput(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}
	disabled := NewWithOptions(store, assets, "http://127.0.0.1:3001", Options{})
	registered := request(disabled, http.MethodPost, "/api/auth/register", `{"company":"Disabled","email":"tron-disabled@example.com","password":"long-test-password-42"}`, nil)
	cookie := registered.Result().Cookies()[0]
	status := request(disabled, http.MethodGet, "/api/payments/status", "", cookie)
	if status.Code != http.StatusOK || status.Body.String() != "{\"enabled\":false,\"stripe\":false,\"tron\":false}\n" {
		t.Fatalf("disabled status: %d %s", status.Code, status.Body.String())
	}
	if rec := serve(disabled, newTronRequest(http.MethodPost, "/api/payments/tron/orders", `{"amountCents":500}`, cookie, "tron-disabled-order")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled create: %d %s", rec.Code, rec.Body.String())
	}

	enabled := NewWithOptions(store, assets, "http://127.0.0.1:3001", Options{TronPayments: NewTronPayments(store, "TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw")})
	for name, req := range map[string]*http.Request{
		"missing key": newTronRequest(http.MethodPost, "/api/payments/tron/orders", `{"amountCents":500}`, cookie, ""),
		"unknown":     newTronRequest(http.MethodPost, "/api/payments/tron/orders", `{"amountCents":500,"extra":true}`, cookie, "tron-strict-order"),
		"query":       newTronRequest(http.MethodPost, "/api/payments/tron/orders?x=1", `{"amountCents":500}`, cookie, "tron-query-order"),
	} {
		if rec := serve(enabled, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
