package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPreflightBoundToCurrentNodeCertificate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "preflight@example.com")
	s.SetPlatformAdmin(ctx, who.Email, true)
	fp := strings.Repeat("c", 64)
	if err := s.EnrollNode(ctx, "hk-01", fp); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"os":"linux","forwarding":"unknown"}`)
	if err := s.ReportPreflight(ctx, "hk-01", raw, strings.Repeat("a", 64)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := s.ReportPreflight(ctx, "hk-01", raw, fp); err != nil {
		t.Fatal(err)
	}
	got, err := s.AdminPreflight(ctx, who, "hk-01")
	if err != nil || got.ReportedAt == nil || len(got.Report) == 0 {
		t.Fatalf("report missing: %v", err)
	}
	if err := s.EnrollNode(ctx, "hk-01", strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	got, err = s.AdminPreflight(ctx, who, "hk-01")
	if err != nil || got.ReportedAt != nil {
		t.Fatal("old identity report not invalidated")
	}
}
