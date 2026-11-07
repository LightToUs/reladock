package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedPKIRefusesToReplaceLostIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "managed")
	if err := s.EnsureManagedNodePKI(ctx, dir, "console.reladock.com"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "issuer", "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureManagedNodePKI(ctx, dir, "console.reladock.com"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "issuer", "ca.key"))
	if err != nil || string(before) != string(after) {
		t.Fatal("existing identity changed")
	}
	// Moving a test-only bundle simulates lost runtime storage; production code must not replace it.
	if err = os.Rename(dir, dir+"-backup"); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureManagedNodePKI(ctx, dir, "console.reladock.com"); err == nil {
		t.Fatal("lost PKI was silently regenerated")
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("rejected initialization wrote files")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=NULL WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO platform_audit_logs(actor_email,action,resource_id,details) VALUES('operator','node.revoke-certificate','hk-01','{}')`); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureManagedNodePKI(ctx, dir, "console.reladock.com"); err == nil {
		t.Fatal("revoked identity history ignored")
	}
}
