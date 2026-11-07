package web

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAdminNodeVisibilityHTTP(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("tenant")}, "admin.html": &fstest.MapFile{Data: []byte("platform admin")}}
	h := New(store, fs.FS(assets), "http://127.0.0.1:3001")

	reg := request(h, http.MethodPost, "/api/auth/register", `{"company":"Operator","email":"visibility-operator@example.com","password":"long-test-password-42"}`, nil)
	if reg.Code != http.StatusCreated {
		t.Fatal(reg.Body.String())
	}
	if err := store.SetPlatformAdmin(context.Background(), "visibility-operator@example.com", true); err != nil {
		t.Fatal(err)
	}
	login := request(h, http.MethodPost, "/api/auth/login", `{"email":"visibility-operator@example.com","password":"long-test-password-42"}`, nil)
	adminCookie := login.Result().Cookies()[0]

	if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", `{"visibilityRevision":1,"hidden":true,"force":true}`, adminCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("visibility accepted unknown field: %d %s", rec.Code, rec.Body.String())
	}
	for name, body := range map[string]string{
		"missing hidden": `{"visibilityRevision":1}`,
		"null hidden":    `{"visibilityRevision":1,"hidden":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", body, adminCookie); rec.Code != http.StatusBadRequest {
				t.Fatalf("visibility accepted missing boolean: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	other := request(h, http.MethodPost, "/api/auth/register", `{"company":"Customer","email":"visibility-customer@example.com","password":"long-test-password-42"}`, nil)
	if other.Code != http.StatusCreated {
		t.Fatal(other.Body.String())
	}
	if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", `{"visibilityRevision":1,"hidden":true}`, other.Result().Cookies()[0]); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant changed node visibility: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", `{"visibilityRevision":1,"hidden":true}`, adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("hide failed: %d %s", rec.Code, rec.Body.String())
	}
	listed := request(h, http.MethodGet, "/api/admin/nodes", "", adminCookie)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"hk-01"`) || !strings.Contains(listed.Body.String(), `"hidden":true`) || !strings.Contains(listed.Body.String(), `"visibilityRevision":2`) {
		t.Fatalf("hidden node facts missing: %d %s", listed.Code, listed.Body.String())
	}
	if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", `{"visibilityRevision":1,"hidden":false}`, adminCookie); rec.Code != http.StatusConflict {
		t.Fatalf("stale restore accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, http.MethodPost, "/api/admin/nodes/hk-01/visibility", `{"visibilityRevision":2,"hidden":false}`, adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", rec.Code, rec.Body.String())
	}
	listed = request(h, http.MethodGet, "/api/admin/nodes", "", adminCookie)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"hidden":false`) || !strings.Contains(listed.Body.String(), `"visibilityRevision":3`) {
		t.Fatalf("restored node facts missing: %d %s", listed.Code, listed.Body.String())
	}
}
