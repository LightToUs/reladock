package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"reladock/internal/platform"
)

func TestSourceIPBlocksHTTPIsAdminOnlyStrictAndVersioned(t *testing.T) {
	store, handler := sourceLoggingHTTPServer(t)
	registered := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Block Admin","email":"block-http-admin@example.com","password":"long-test-password-42"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	tenantCookie := registered.Result().Cookies()[0]
	who, err := store.Session(context.Background(), tenantCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	created := request(handler, http.MethodPost, "/api/rules", `{"name":"Relay","protocol":"udp","targetHost":"198.51.100.20","targetPort":443,"bandwidthMbps":100,"nodeId":"hk-01"}`, tenantCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var rule platform.Rule
	if err = json.Unmarshal(created.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	nonAdmin := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Other Tenant","email":"block-http-other@example.com","password":"long-test-password-42"}`, nil)
	nonAdminCookie := nonAdmin.Result().Cookies()[0]
	if err = store.SetPlatformAdmin(context.Background(), who.Email, true); err != nil {
		t.Fatal(err)
	}
	loggedIn := request(handler, http.MethodPost, "/api/auth/login", `{"email":"block-http-admin@example.com","password":"long-test-password-42"}`, nil)
	adminCookie := loggedIn.Result().Cookies()[0]
	path := "/api/admin/rules/" + rule.ID + "/source-blocks"
	body := `{"tenantId":"` + who.TenantID + `","cidr":"198.51.100.23","reason":"abuse report","desiredVersion":1}`

	if rec := request(handler, http.MethodPost, path, body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(handler, http.MethodPost, path, body, nonAdminCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(handler, http.MethodPost, path, body[:len(body)-1]+`,"extra":true}`, adminCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body.String())
	}
	rec := request(handler, http.MethodPost, path, body, adminCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var block platform.SourceIPBlock
	if err = json.Unmarshal(rec.Body.Bytes(), &block); err != nil || block.CIDR != "198.51.100.23/32" || block.RequestedVersion != 2 {
		t.Fatalf("block=%+v err=%v", block, err)
	}
	list := request(handler, http.MethodGet, path+"?tenantId="+who.TenantID, "", adminCookie)
	var blocks platform.SourceIPBlockPage
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &blocks) != nil || len(blocks.Items) != 1 || blocks.Items[0].ID != block.ID || blocks.DesiredVersion != 2 {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	if stale := request(handler, http.MethodPost, path, body, adminCookie); stale.Code != http.StatusConflict {
		t.Fatalf("stale add: %d %s", stale.Code, stale.Body.String())
	}
	if cross := request(handler, http.MethodGet, path+"?tenantId="+newHTTPUUID(), "", adminCookie); cross.Code != http.StatusNotFound {
		t.Fatalf("cross tenant list: %d %s", cross.Code, cross.Body.String())
	}
	removePath := path + "/" + block.ID
	removed := request(handler, http.MethodDelete, removePath, `{"tenantId":"`+who.TenantID+`","desiredVersion":2}`, adminCookie)
	if removed.Code != http.StatusOK || json.Unmarshal(removed.Body.Bytes(), &block) != nil || block.State != "removed" || block.RemovedVersion != 3 {
		t.Fatalf("remove: %d %s", removed.Code, removed.Body.String())
	}
}

func newHTTPUUID() string { return "99999999-9999-4999-8999-999999999999" }
