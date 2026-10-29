package platform

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBootstrapGrantLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "bootstrap@example.com")
	if _, err := s.CreateNodeBootstrap(ctx, who, "hk-01", 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	grant, err := s.CreateNodeBootstrap(ctx, who, "hk-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.Token) < 40 || time.Until(grant.ExpiresAt) > 16*time.Minute {
		t.Fatal("unsafe token lifetime")
	}
	sign := func(id string, csr []byte) ([]byte, error) {
		return []byte("certificate:" + id + ":" + string(csr)), nil
	}
	// Store accepts only caller-validated CSR, and a signer result fingerprint binds replay.
	var hash string
	if err = s.pool.QueryRow(ctx, "SELECT token_hash FROM node_bootstrap WHERE node_id='hk-01'").Scan(&hash); err != nil || hash == grant.Token {
		t.Fatalf("stored plaintext: %v", err)
	}
	cert, err := s.RedeemNodeBootstrap(ctx, grant.Token, []byte("csr-a"), sign)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RedeemNodeBootstrap(ctx, grant.Token, []byte("csr-a"), sign)
	if err != nil || string(cert.Certificate) != string(again.Certificate) {
		t.Fatalf("recovery: %v", err)
	}
	if _, err = s.RedeemNodeBootstrap(ctx, grant.Token, []byte("csr-b"), sign); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("second identity: %v", err)
	}
	if err = s.EnrollNode(ctx, "hk-01", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RedeemNodeBootstrap(ctx, grant.Token, []byte("csr-a"), sign); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked restored: %v", err)
	}
}

func TestBootstrapConcurrentAndExpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "concurrent-bootstrap@example.com")
	if err := s.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateNodeBootstrap(ctx, who, "hk-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	successes := make(chan bool, 2)
	for _, csr := range []string{"one", "two"} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			_, e := s.RedeemNodeBootstrap(ctx, g.Token, []byte(c), func(id string, b []byte) ([]byte, error) { return append([]byte(id), b...), nil })
			successes <- e == nil
		}(csr)
	}
	wg.Wait()
	close(successes)
	n := 0
	for ok := range successes {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("identities issued: %d", n)
	}
	var rev int64
	s.pool.QueryRow(ctx, "SELECT revision FROM nodes WHERE id='hk-01'").Scan(&rev)
	g, err = s.CreateNodeBootstrap(ctx, who, "hk-01", rev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE node_bootstrap SET expires_at=now()-interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RedeemNodeBootstrap(ctx, g.Token, []byte("x"), func(string, []byte) ([]byte, error) { t.Fatal("expired token signed"); return nil, nil }); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
