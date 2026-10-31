package recovery

import (
	"strings"
	"testing"
)

func TestPostgresEnvironmentKeepsPasswordOutOfArguments(t *testing.T) {
	env, cfg, err := postgresConnection("postgres://alice:s3cr3t@db.example:5433/reladock?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PGPASSWORD=s3cr3t") || !strings.Contains(joined, "PGSSLMODE=require") || cfg.Database != "reladock" {
		t.Fatalf("env/config not populated")
	}
	for _, arg := range postgresToolArgs("dump", "snapshot", "/tmp/out") {
		if strings.Contains(arg, "s3cr3t") {
			t.Fatal("password leaked into arguments")
		}
	}
}

func TestInvalidDatabaseURLDoesNotEchoSecret(t *testing.T) {
	_, _, err := postgresConnection("http://alice:very-secret@example/reladock")
	if err == nil {
		t.Fatal("accepted non-PostgreSQL URL")
	}
	if strings.Contains(err.Error(), "very-secret") {
		t.Fatalf("secret in error: %v", err)
	}
}

func TestInvariantMismatchIsRejected(t *testing.T) {
	want := map[string]string{"schemaVersion": "23", "walletCount": "2", "ledgerCount": "4", "walletMismatchCount": "0"}
	got := map[string]string{"schemaVersion": "23", "walletCount": "2", "ledgerCount": "3", "walletMismatchCount": "0"}
	if err := compareInvariants(want, got); err == nil {
		t.Fatal("accepted mismatched ledger invariant")
	}
	got["ledgerCount"] = "4"
	if err := compareInvariants(want, got); err != nil {
		t.Fatal(err)
	}
}

func TestTemporaryDatabaseNameIsBounded(t *testing.T) {
	for i := 0; i < 10; i++ {
		n, err := temporaryDatabaseName()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(n, "reladock_verify_") || len(n) > 63 {
			t.Fatalf("unsafe name %q", n)
		}
	}
}
