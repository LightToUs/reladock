package platform

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"reladock/internal/nodeauth"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdminCertificateRenewalRequestIsPendingIdempotentAndRevisionGuarded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	old := strings.Repeat("a", 64)
	var initialGeneration int64
	if err := s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id='hk-01'").Scan(&initialGeneration); err != nil {
		t.Fatal(err)
	}
	setRotationTestIdentity(t, s, old)
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); err != nil {
		t.Fatalf("request renewal: %v", err)
	}
	var current, desired string
	var revision, generation int64
	read := func() {
		t.Helper()
		if err := s.pool.QueryRow(ctx, "SELECT certificate_fingerprint,certificate_renewal_fingerprint,revision,desired_generation FROM nodes WHERE id='hk-01'").Scan(&current, &desired, &revision, &generation); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if current != old || desired != old || revision != 2 || generation != initialGeneration {
		t.Fatalf("request changed identity/config: current=%s desired=%s revision=%d generation=%d", current, desired, revision, generation)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 2, "rotate-certificate", ""); err != nil {
		t.Fatalf("pending replay: %v", err)
	}
	read()
	var audits int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE action='node.certificate-renew-requested'").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || audits != 1 {
		t.Fatalf("replay repeated mutation: revision=%d audits=%d", revision, audits)
	}
}

func TestAdminCertificateRenewalRejectsUntrustedUnsupportedAndStaleRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	tenant, _ := register(t, s, "renew-tenant@example.com")
	old := strings.Repeat("b", 64)
	setRotationTestIdentity(t, s, old)
	for _, tc := range []struct {
		name string
		who  Identity
		id   string
		rev  int64
		cert string
		want error
	}{
		{"tenant", tenant, "hk-01", 1, "", ErrForbidden},
		{"invalid revision", admin, "hk-01", 0, "", ErrInvalid},
		{"stale", admin, "hk-01", 2, "", ErrConflict},
		{"unknown node", admin, "absent", 1, "", ErrNotFound},
		{"certificate payload", admin, "hk-01", 1, "untrusted", ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.AdminNodeAction(ctx, tc.who, tc.id, tc.rev, "rotate-certificate", tc.cert); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	for _, status := range []string{"draining", "retired"} {
		if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status=$1 WHERE id='hk-01'", status); err != nil {
			t.Fatal(err)
		}
		if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: %v", status, err)
		}
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status='ready',agent_certificate_rotation_supported=false WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("old Agent: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=NULL,agent_certificate_rotation_supported=true WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("no identity: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, admin.Email, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked admin: %v", err)
	}
}

func TestAdminCertificateRenewalConcurrentRequestsAndAuditRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	setRotationTestIdentity(t, s, strings.Repeat("c", 64))
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_renewal_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='node.certificate-renew-requested' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_renewal_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_renewal_audit()`); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); err == nil {
		t.Fatal("audit failure accepted")
	}
	var pending bool
	var revision int64
	if err := s.pool.QueryRow(ctx, "SELECT certificate_renewal_fingerprint IS NOT NULL,revision FROM nodes WHERE id='hk-01'").Scan(&pending, &revision); err != nil || pending || revision != 1 {
		t.Fatalf("rollback pending=%v rev=%d err=%v", pending, revision, err)
	}
	if _, err := s.pool.Exec(ctx, "DROP TRIGGER reject_renewal_audit ON platform_audit_logs"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", "") }()
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successes=%d", successes)
	}
}

func TestCertificateRenewalIntentSurvivesIssueAndClearsOnIdentityChange(t *testing.T) {
	for _, action := range []string{"activate", "enroll", "revoke", "bootstrap"} {
		t.Run(action, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _ := adminIdentity(t, s)
			old := strings.Repeat("d", 64)
			setRotationTestIdentity(t, s, old)
			if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); err != nil {
				t.Fatal(err)
			}
			signer := rotationSigner(t)
			issued, err := s.RenewNodeCertificate(ctx, "hk-01", old, ed25519CSR(t), signer.Sign)
			if err != nil {
				t.Fatal(err)
			}
			if desired, err := s.NodeCertificateRenewalRequest(ctx, "hk-01", old); err != nil || desired != old {
				t.Fatalf("issue cleared request: %q %v", desired, err)
			}
			block, _ := pem.Decode(issued.Certificate)
			leaf, _ := x509.ParseCertificate(block.Bytes)
			next := nodeauth.Fingerprint(leaf)
			switch action {
			case "activate":
				err = s.ActivateNodeCertificate(ctx, "hk-01", next)
			case "enroll":
				err = s.EnrollNode(ctx, "hk-01", strings.Repeat("e", 64))
			case "revoke":
				err = s.AdminNodeAction(ctx, admin, "hk-01", 2, "revoke-certificate", "")
			case "bootstrap":
				var g BootstrapGrant
				g, err = s.CreateNodeBootstrap(ctx, admin, "hk-01", 2)
				if err == nil {
					_, err = s.RedeemNodeBootstrap(ctx, g.Token, ed25519CSR(t), signer.Sign)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			var pending bool
			if err = s.pool.QueryRow(ctx, "SELECT certificate_renewal_fingerprint IS NOT NULL FROM nodes WHERE id='hk-01'").Scan(&pending); err != nil || pending {
				t.Fatalf("identity replacement retained request: %v %v", pending, err)
			}
			if _, err = s.NodeCertificateRenewalRequest(ctx, "hk-01", old); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("old cert can read command: %v", err)
			}
			if action == "activate" {
				if err = s.ActivateNodeCertificate(ctx, "hk-01", next); err != nil {
					t.Fatalf("activation replay: %v", err)
				}
			}
		})
	}
}

func TestCertificateRotationCapabilityUsesLatestHeartbeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	fp := strings.Repeat("f", 64)
	if err := s.EnrollNode(ctx, "hk-01", fp); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.CertificateRotationSupported = true
	for _, supported := range []bool{true, false} {
		h.CertificateRotationSupported = supported
		if err := s.ReportAgentHeartbeat(ctx, "hk-01", fp, h); err != nil {
			t.Fatal(err)
		}
		nodes, err := s.AdminNodes(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.ID == "hk-01" && n.CertificateRotationSupported != supported {
				t.Fatalf("capability not updated: %+v", n)
			}
		}
	}
	// Capability belongs to the authenticated identity; replacement must wait
	// for its own heartbeat, including when a node is rebound to an old binary.
	h.CertificateRotationSupported = true
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fp, h); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollNode(ctx, "hk-01", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var supported bool
	if err := s.pool.QueryRow(ctx, "SELECT agent_certificate_rotation_supported FROM nodes WHERE id='hk-01'").Scan(&supported); err != nil || supported {
		t.Fatalf("replacement inherited capability: %v %v", supported, err)
	}
}

func setRotationTestIdentity(t *testing.T, s *Store, fingerprint string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), "UPDATE nodes SET agent_certificate_rotation_supported=true WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
}

func TestMigration33PreservesIdentityAndGuardsRenewalTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fp := strings.Repeat("a", 64)
	if err := s.EnrollNode(ctx, "hk-01", fp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER clear_node_certificate_renewal ON nodes; DROP FUNCTION clear_node_certificate_renewal(); ALTER TABLE nodes DROP COLUMN certificate_renewal_fingerprint,DROP COLUMN agent_certificate_rotation_supported; DELETE FROM schema_migrations WHERE version=33`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var current string
	var requested, supported bool
	if err := s.pool.QueryRow(ctx, "SELECT certificate_fingerprint,certificate_renewal_fingerprint IS NOT NULL,agent_certificate_rotation_supported FROM nodes WHERE id='hk-01'").Scan(&current, &requested, &supported); err != nil || current != fp || requested || supported {
		t.Fatalf("migration changed identity/opt-in: %s %v %v %v", current, requested, supported, err)
	}
	for _, bad := range []string{"invalid", strings.Repeat("b", 64)} {
		if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_renewal_fingerprint=$1 WHERE id='hk-01'", bad); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
}

func TestCertificateActivationAuditFailurePreservesRequestedIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	old := strings.Repeat("a", 64)
	setRotationTestIdentity(t, s, old)
	if err := s.AdminNodeAction(ctx, admin, "hk-01", 1, "rotate-certificate", ""); err != nil {
		t.Fatal(err)
	}
	issued, err := s.RenewNodeCertificate(ctx, "hk-01", old, ed25519CSR(t), rotationSigner(t).Sign)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	next := nodeauth.Fingerprint(leaf)
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_activation_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='node.certificate-renew-activated' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_activation_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_activation_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateNodeCertificate(ctx, "hk-01", next); err == nil {
		t.Fatal("activation accepted audit failure")
	}
	if desired, err := s.NodeCertificateRenewalRequest(ctx, "hk-01", old); err != nil || desired != old {
		t.Fatalf("failed activation lost current identity/request: %s %v", desired, err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", next); err != nil {
		t.Fatalf("pending identity lost: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_activation_audit ON platform_audit_logs"); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateNodeCertificate(ctx, "hk-01", next); err != nil {
		t.Fatal(err)
	}
}
