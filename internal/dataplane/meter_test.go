package dataplane

import (
	"context"
	"path/filepath"
	"reladock/internal/metering"
	"testing"
)

func TestMeterHooksBindPhysicalCountersToRuleAndPaidPeriod(t *testing.T) {
	ctx := context.Background()
	w, e := metering.Open(filepath.Join(t.TempDir(), "wal"), metering.Options{NodeID: "hk-01"})
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	cfg := fixture()
	cfg.Rules[0].ID = "00000000-0000-4000-8000-000000000001"
	cfg.Rules[0].SubscriptionID = "00000000-0000-4000-8000-000000000002"
	p, e := Compile(ctx, cfg, Options{NodeID: "hk-01", SNATAddress: "192.0.2.2", incarnation: "kernel-test"})
	if e != nil {
		t.Fatal(e)
	}
	raw := `{"nftables":[`
	for i, c := range p.Counters {
		if i > 0 {
			raw += ","
		}
		raw += `{"counter":{"family":"ip","table":"reladock","name":"` + c.Name + `","bytes":100,"packets":2}}`
	}
	raw += `]}`
	h := MeterHooks{WAL: w, Runner: counterRunner{raw: raw}}
	if e = h.AfterReplace(ctx, p); e != nil {
		t.Fatal(e)
	}
	first, ok, e := w.Next()
	if e != nil || !ok || first.Report.UploadBytes != 200 || first.Report.DownloadBytes != 200 || first.Report.SubscriptionID != cfg.Rules[0].SubscriptionID {
		t.Fatalf("wrong billed direction/period: %+v %v", first, e)
	}
	if e = h.BeforeReplace(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e = w.Ack(ctx, metering.Receipt{RuleID: first.Report.RuleID, SubscriptionID: first.Report.SubscriptionID, Epoch: first.Report.Epoch, Sequence: first.Report.Sequence}); e != nil {
		t.Fatal(e)
	}
	if _, ok, e = w.Next(); e != nil || ok {
		t.Fatal("same kernel counters double-counted")
	}
}

type counterRunner struct{ raw string }

func (r counterRunner) Run(context.Context, []string, string) ([]byte, error) {
	return []byte(r.raw), nil
}
