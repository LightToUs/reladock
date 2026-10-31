package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAdminNodeDrainingEndpointIsRevisionGuardedAndIrreversible(t *testing.T) {
	store := testWebStore(t)
	ctx := context.Background()
	who, _, err := store.Register(ctx, "Retirement admin", "retirement-admin@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	who, _, err = store.Login(ctx, who.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AdminNodeAction(ctx, who, "hk-01", 1, "maintenance", ""); err != nil {
		t.Fatal(err)
	}
	h := New(store, nil, "http://127.0.0.1:3001")
	login := request(h, "POST", "/api/auth/login", `{"email":"retirement-admin@example.com","password":"long-test-password-42"}`, nil)
	cookie := login.Result().Cookies()[0]
	if response := request(h, "POST", "/api/admin/nodes/hk-01/start-draining", `{"revision":1}`, cookie); response.Code != http.StatusConflict {
		t.Fatalf("stale draining revision: %d %s", response.Code, response.Body.String())
	}
	if response := request(h, "POST", "/api/admin/nodes/hk-01/start-draining", `{"revision":2}`, cookie); response.Code != http.StatusOK {
		t.Fatalf("start draining: %d %s", response.Code, response.Body.String())
	}
	listed := request(h, "GET", "/api/admin/nodes", "", cookie)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"status":"draining"`) || !strings.Contains(listed.Body.String(), `"retirementEligible":false`) {
		t.Fatalf("draining presentation facts: %d %s", listed.Code, listed.Body.String())
	}
	if response := request(h, "POST", "/api/admin/nodes/hk-01/finalize-retirement", `{"revision":3}`, cookie); response.Code != http.StatusConflict {
		t.Fatalf("ungated finalize: %d %s", response.Code, response.Body.String())
	}
}
