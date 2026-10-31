package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func postgresConnection(raw string) ([]string, *pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(raw)
	if err != nil || !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil, nil, errors.New("invalid PostgreSQL DATABASE_URL")
	}
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil {
		return nil, nil, errors.New("invalid PostgreSQL DATABASE_URL")
	}
	blocked := map[string]bool{"PGHOST": true, "PGPORT": true, "PGUSER": true, "PGPASSWORD": true, "PGDATABASE": true, "PGSSLMODE": true, "PGSSLCERT": true, "PGSSLKEY": true, "PGSSLROOTCERT": true, "PGSSLCRL": true, "PGTARGETSESSIONATTRS": true, "PGCONNECT_TIMEOUT": true, "PGOPTIONS": true, "PGAPPNAME": true}
	env := make([]string, 0, len(os.Environ())+5)
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if !blocked[key] {
			env = append(env, v)
		}
	}
	env = append(env, "PGHOST="+config.Host, "PGPORT="+strconv.Itoa(int(config.Port)), "PGUSER="+config.User, "PGPASSWORD="+config.Password, "PGDATABASE="+config.Database)
	for query, variable := range map[string]string{"sslmode": "PGSSLMODE", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "sslrootcert": "PGSSLROOTCERT", "sslcrl": "PGSSLCRL", "target_session_attrs": "PGTARGETSESSIONATTRS", "connect_timeout": "PGCONNECT_TIMEOUT", "options": "PGOPTIONS", "application_name": "PGAPPNAME"} {
		if value := parsed.Query().Get(query); value != "" {
			env = append(env, variable+"="+value)
		}
	}
	return env, config, nil
}

func postgresToolArgs(kind, snapshot, file string) []string {
	switch kind {
	case "dump":
		return []string{"--format=custom", "--no-owner", "--no-privileges", "--snapshot=" + snapshot, "--file=" + file}
	case "restore":
		return []string{"--exit-on-error", "--no-owner", "--no-privileges", "--dbname", snapshot, file}
	default:
		return nil
	}
}

func temporaryDatabaseName() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "reladock_verify_" + hex.EncodeToString(b[:]), nil
}

func commandError(name string, stderr *limitBuffer, err error) error {
	if stderr.String() != "" {
		return fmt.Errorf("%s failed: %s", name, stderr.String())
	}
	return fmt.Errorf("%s failed: %w", name, err)
}

func runPostgresTool(ctx context.Context, binary string, args, env []string) error {
	if err := regularAbsolute(binary); err != nil {
		return fmt.Errorf("invalid PostgreSQL tool: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	var stderr limitBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return commandError("PostgreSQL tool", &stderr, err)
	}
	return nil
}

func withDatabase(env []string, database string) []string {
	out := append([]string(nil), env...)
	for i, v := range out {
		if strings.HasPrefix(v, "PGDATABASE=") {
			out[i] = "PGDATABASE=" + database
			return out
		}
	}
	return append(out, "PGDATABASE="+database)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func collectInvariants(ctx context.Context, q rowQuerier) (map[string]string, error) {
	var schema, wallets, ledger, mismatch int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM schema_migrations`).Scan(&schema); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM billing_wallets`).Scan(&wallets); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM billing_ledger`).Scan(&ledger); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM billing_wallets w WHERE w.balance_cents<>COALESCE((SELECT l.balance_after_cents FROM billing_ledger l WHERE l.tenant_id=w.tenant_id ORDER BY l.id DESC LIMIT 1),0)`).Scan(&mismatch); err != nil {
		return nil, err
	}
	return map[string]string{"schemaVersion": strconv.FormatInt(schema, 10), "walletCount": strconv.FormatInt(wallets, 10), "ledgerCount": strconv.FormatInt(ledger, 10), "walletMismatchCount": strconv.FormatInt(mismatch, 10)}, nil
}

func compareInvariants(want, got map[string]string) error {
	for _, key := range []string{"schemaVersion", "walletCount", "ledgerCount", "walletMismatchCount"} {
		if want[key] != got[key] {
			return fmt.Errorf("restored %s invariant does not match bundle", key)
		}
	}
	if got["walletMismatchCount"] != "0" {
		return errors.New("restored wallet ledger invariant is inconsistent")
	}
	return nil
}
