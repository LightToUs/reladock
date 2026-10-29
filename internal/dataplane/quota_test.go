package dataplane

import (
	"context"
	"strings"
	"testing"
)

func TestRuleQuotaIsSharedAcrossBothDirectionsAndProtocolsNotAcrossRules(t *testing.T) {
	cfg := fixture()
	cfg.Rules[0].QuotaBytes = 1000
	cfg.Rules[0].UsageUploadBytes = 100
	cfg.Rules[0].UsageDownloadBytes = 200
	p, e := Compile(context.Background(), cfg, options())
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(p.Script, "quota q_") != 1 || !strings.Contains(p.Script, "until 700 bytes") || strings.Count(p.Script, "quota name q_") != 4 {
		t.Fatal("quota isn't per rule combined", p.Script)
	}
	o := options()
	o.quotaUsed = map[string]int64{"rule-a": 900}
	p, e = Compile(context.Background(), cfg, o)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(p.Script, "until 100 bytes") {
		t.Fatal("configuration rebuild renewed local usage", p.Script)
	}
	o.quotaUsed["rule-a"] = 1000
	p, e = Compile(context.Background(), cfg, o)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(p.Script, "counter name") {
		t.Fatal("exhausted quota still forwards")
	}
}
