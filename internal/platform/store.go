package platform

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid      = errors.New("输入参数无效")
	ErrConflict     = errors.New("资源或幂等请求冲突")
	ErrUnauthorized = errors.New("邮箱或密码错误，或登录已失效")
	ErrNotFound     = errors.New("资源不存在或无权访问")
	ErrForbidden    = errors.New("没有操作权限")
	ErrCapacity     = errors.New("节点可用端口不足")
)

type Store struct {
	pool                 *pgxpool.Pool
	allowUnentitledRules bool
}

type StoreOptions struct {
	AllowUnentitledRules bool
}

//go:embed schema.sql
var schemaSQL string

//go:embed migration18.sql
var migration18SQL string

//go:embed migration2.sql
var migration2SQL string

//go:embed migration3.sql
var migration3SQL string

//go:embed migration4.sql
var migration4SQL string

//go:embed migration5.sql
var migration5SQL string

//go:embed migration6.sql
var migration6SQL string

//go:embed migration7.sql
var migration7SQL string

//go:embed migration8.sql
var migration8SQL string

//go:embed migration9.sql
var migration9SQL string

//go:embed migration10.sql
var migration10SQL string

//go:embed migration12.sql
var migration12SQL string

//go:embed migration13.sql
var migration13SQL string

//go:embed migration14.sql
var migration14SQL string

//go:embed migration16.sql
var migration16SQL string

const SchemaVersion = 18

func Open(ctx context.Context, url string) (*Store, error) {
	return OpenWithOptions(ctx, url, StoreOptions{})
}

func OpenWithOptions(ctx context.Context, url string, options StoreOptions) (*Store, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required; no JSON or memory fallback")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = 12
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, allowUnentitledRules: options.AllowUnentitledRules}
	if err = pool.Ping(ctx); err == nil {
		err = s.Migrate(ctx)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7263520181)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return errors.New("database schema is newer than this application")
	}
	if version == 0 {
		if _, err = tx.Exec(ctx, schemaSQL); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(1)"); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.Exec(ctx, migration2SQL); err != nil {
			return fmt.Errorf("migration 2: %w", err)
		}
		rows, err := tx.Query(ctx, "SELECT id FROM nodes ORDER BY id")
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err = s.enqueueConfig(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(2)"); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.Exec(ctx, migration3SQL); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(3)"); err != nil {
			return err
		}
	}
	for _, migration := range []struct {
		version int
		sql     string
	}{{4, migration4SQL}, {5, migration5SQL}, {6, migration6SQL}, {7, migration7SQL}, {8, migration8SQL}, {9, migration9SQL}, {10, migration10SQL}} {
		if version < migration.version {
			if _, err = tx.Exec(ctx, migration.sql); err != nil {
				return fmt.Errorf("migration %d: %w", migration.version, err)
			}
			if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", migration.version); err != nil {
				return err
			}
		}
	}
	if version < 11 {
		rows, e := tx.Query(ctx, "SELECT id FROM nodes ORDER BY id")
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			if e = lockNode(ctx, tx, id); e != nil {
				return e
			}
			if e = s.enqueueConfig(ctx, tx, id); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(11)"); e != nil {
			return e
		}
	}
	if version < 12 {
		if _, err = tx.Exec(ctx, migration12SQL); err != nil {
			return fmt.Errorf("migration 12: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(12)"); err != nil {
			return err
		}
	}
	if version < 13 {
		if _, err = tx.Exec(ctx, migration13SQL); err != nil {
			return fmt.Errorf("migration 13: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(13)"); err != nil {
			return err
		}
	}
	if version < 14 {
		if _, err = tx.Exec(ctx, migration14SQL); err != nil {
			return fmt.Errorf("migration 14: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(14)"); err != nil {
			return err
		}
	}
	for _, migration := range []struct {
		version int
		sql     string
	}{{15, NotificationMigrationSQL}, {16, migration16SQL}} {
		if version < migration.version {
			if _, err = tx.Exec(ctx, migration.sql); err != nil {
				return fmt.Errorf("migration %d: %w", migration.version, err)
			}
			if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", migration.version); err != nil {
				return err
			}
		}
	}
	if version < 17 {
		// Older validated snapshots lack local quota/expiry metadata. Reissue
		// immutable full snapshots; never adopt their validation as application.
		rows, e := tx.Query(ctx, "SELECT id FROM nodes ORDER BY id")
		if e != nil {
			return e
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			if e = lockNode(ctx, tx, id); e != nil {
				return e
			}
			if e = s.enqueueConfig(ctx, tx, id); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(17)"); e != nil {
			return e
		}
	}
	if version < 18 {
		if _, err = tx.Exec(ctx, migration18SQL); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(18)"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func audit(ctx context.Context, tx pgx.Tx, who Identity, action, resource string) error {
	_, err := tx.Exec(ctx, "INSERT INTO audit_logs(tenant_id,actor_id,action,resource_id) VALUES($1,$2,$3,$4)", who.TenantID, who.MemberID, action, resource)
	return err
}
