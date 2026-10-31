package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/platform"
)

type ControlCreateConfig struct{ DatabaseURL, Destination, Recipient, AgeBinary, PGDumpBinary string }
type ControlVerifyConfig struct{ DatabaseURL, Bundle, Identity, AgeBinary, PGRestoreBinary string }

func sourceID(config *pgx.ConnConfig) string {
	sum := sha256.Sum256([]byte(config.Host + ":" + strconv.Itoa(int(config.Port)) + "/" + config.Database))
	return hex.EncodeToString(sum[:16])
}

func CreateControl(ctx context.Context, c ControlCreateConfig) (string, error) {
	env, dbconfig, err := postgresConnection(c.DatabaseURL)
	if err != nil {
		return "", err
	}
	if err = regularAbsolute(c.PGDumpBinary); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp("", "reladock-control-create-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	if err = os.Chmod(temp, 0700); err != nil {
		return "", err
	}
	dump := filepath.Join(temp, "control.dump")
	conn, err := pgx.ConnectConfig(ctx, dbconfig)
	if err != nil {
		return "", errors.New("connect to control database failed")
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	invariants, err := collectInvariants(ctx, tx)
	if err != nil {
		return "", err
	}
	if invariants["walletMismatchCount"] != "0" {
		return "", errors.New("source wallet ledger invariant is inconsistent")
	}
	var snapshot string
	if err = tx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		return "", err
	}
	if err = runPostgresTool(ctx, c.PGDumpBinary, postgresToolArgs("dump", snapshot, dump), env); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	committed = true
	m := Manifest{Version: FormatVersion, Kind: KindControl, CreatedAt: time.Now().UTC(), SourceID: sourceID(dbconfig), Invariants: invariants}
	return Encrypt(ctx, c.AgeBinary, c.Destination, KindControl, c.Recipient, func(w io.Writer) error {
		_, e := WriteBundle(w, m, []SourceFile{{Path: "control.dump", Source: dump, Mode: 0600}})
		return e
	})
}

func VerifyControl(ctx context.Context, c ControlVerifyConfig) error {
	env, dbconfig, err := postgresConnection(c.DatabaseURL)
	if err != nil {
		return err
	}
	if err = regularAbsolute(c.PGRestoreBinary); err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "reladock-control-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	stage := filepath.Join(root, "stage")
	var manifest Manifest
	if err = Decrypt(ctx, c.AgeBinary, c.Bundle, c.Identity, func(r io.Reader) error {
		var extractErr error
		manifest, extractErr = ExtractBundle(r, stage, KindControl)
		return extractErr
	}); err != nil {
		return err
	}
	if manifest.Invariants["schemaVersion"] != strconv.Itoa(platform.SchemaVersion) {
		return errors.New("bundle schema version does not match this RelaDock release")
	}
	name, err := temporaryDatabaseName()
	if err != nil {
		return err
	}
	adminConfig := dbconfig.Copy()
	adminConfig.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		return errors.New("connect to PostgreSQL administrator database failed")
	}
	defer admin.Close(ctx)
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		return errors.New("create temporary verification database failed")
	}
	defer func() {
		cleanup := context.Background()
		_, _ = admin.Exec(cleanup, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", name)
		_, _ = admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+identifier)
	}()
	if err = runPostgresTool(ctx, c.PGRestoreBinary, postgresToolArgs("restore", name, filepath.Join(stage, "control.dump")), withDatabase(env, name)); err != nil {
		return err
	}
	verifyConfig := dbconfig.Copy()
	verifyConfig.Database = name
	restored, err := pgx.ConnectConfig(ctx, verifyConfig)
	if err != nil {
		return errors.New("connect to restored verification database failed")
	}
	got, queryErr := collectInvariants(ctx, restored)
	closeErr := restored.Close(ctx)
	if queryErr != nil {
		return queryErr
	}
	if closeErr != nil {
		return closeErr
	}
	return compareInvariants(manifest.Invariants, got)
}
