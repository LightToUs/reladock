package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/platform"
)

func TestNotificationsHTTPAdminMetadataAndExhaustedRetry(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	root, e := pgxpool.New(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := "notification_web_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, e = root.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); root.Close() })
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, e := platform.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	seed, e := pgxpool.New(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer seed.Close()
	var exists bool
	if e = seed.QueryRow(ctx, "SELECT to_regclass('notification_outbox') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	if !exists {
		if _, e = seed.Exec(ctx, platform.NotificationMigrationSQL); e != nil {
			t.Fatal(e)
		}
	}
	owner, _, e := store.Register(ctx, "Notification admin", "notification-http@example.com", "long-test-password-42")
	if e != nil {
		t.Fatal(e)
	}
	if e = store.SetPlatformAdmin(ctx, owner.Email, true); e != nil {
		t.Fatal(e)
	}
	admin, _, e := store.Login(ctx, owner.Email, "long-test-password-42")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = seed.Exec(ctx, "INSERT INTO operations_incidents(id,node_id,kind,state) VALUES('19834c51-0b88-4d85-8c17-c2805c9ef532','hk-01','contact-stale','open')"); e != nil {
		t.Fatal(e)
	}
	job, e := store.ClaimNotification(ctx)
	if e != nil || job == nil {
		t.Fatalf("claim %+v %v", job, e)
	}
	if _, e = seed.Exec(ctx, "UPDATE notification_outbox SET state='exhausted',attempts=8,lease_until=NULL,last_error_code='delivery-failed'"); e != nil {
		t.Fatal(e)
	}
	s := &server{store: store}
	call := func(who platform.Identity, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if !s.serveNotifications(w, r, who) {
			t.Fatalf("route unhandled %s", path)
		}
		return w
	}
	if w := call(owner, "GET", "/api/admin/notifications", ""); w.Code != 403 {
		t.Fatalf("nonadmin %d", w.Code)
	}
	forged := owner
	forged.PlatformAdmin = true
	// Same member has now been granted actual platform rights; use a fresh owner
	// to verify a forged flag cannot read the platform metadata.
	foreign, _, e := store.Register(ctx, "Foreign", "notification-foreign@example.com", "long-test-password-42")
	if e != nil {
		t.Fatal(e)
	}
	forged = foreign
	forged.PlatformAdmin = true
	if w := call(forged, "GET", "/api/admin/notifications", ""); w.Code != 403 {
		t.Fatalf("forged admin %d", w.Code)
	}
	w := call(admin, "GET", "/api/admin/notifications?page=1", "")
	if w.Code != 200 {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	var page platform.Page[platform.NotificationDelivery]
	if e = json.Unmarshal(w.Body.Bytes(), &page); e != nil || page.Total != 1 || page.Items[0].State != "exhausted" {
		t.Fatalf("metadata %s %v", w.Body.String(), e)
	}
	for _, bad := range []string{job.LeaseToken, "leaseToken", "payload", "secret"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatalf("metadata leak %s", w.Body.String())
		}
	}
	for _, path := range []string{"/api/admin/notifications?page=0", "/api/admin/notifications?page=1&page=2", "/api/admin/notifications?tenantId=foreign", "/api/admin/notifications?page="} {
		if w := call(admin, "GET", path, ""); w.Code != 400 {
			t.Fatalf("invalid query %s: %d", path, w.Code)
		}
	}
	for _, body := range []string{`{"eventId":"invalid"}`, `{"eventId":"` + job.Event.EventID + `","attempts":0}`, `null`, `{} {}`} {
		if w := call(admin, "POST", "/api/admin/notifications/retry", body); w.Code != 400 {
			t.Fatalf("invalid retry %s: %d", body, w.Code)
		}
	}
	body := `{"eventId":"` + job.Event.EventID + `"}`
	for i := 0; i < 2; i++ {
		if w := call(admin, "POST", "/api/admin/notifications/retry", body); w.Code != 200 {
			t.Fatalf("retry %d %s", w.Code, w.Body.String())
		}
	}
	next, e := store.ClaimNotification(ctx)
	if e != nil || next == nil {
		t.Fatalf("retry claim %+v %v", next, e)
	}
	if e = store.ReportNotification(ctx, next.Event.EventID, next.LeaseToken, true, ""); e != nil {
		t.Fatal(e)
	}
	if w := call(admin, "POST", "/api/admin/notifications/retry", body); w.Code != 409 {
		t.Fatalf("delivered resent %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/elsewhere", nil)
	if s.serveNotifications(httptest.NewRecorder(), r, admin) {
		t.Fatal("claimed unrelated route")
	}
}
