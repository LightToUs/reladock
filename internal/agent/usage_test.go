package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reladock/internal/metering"
	"strings"
	"testing"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUsageSenderKeepsDurablePayloadUntilMatchingReceipt(t *testing.T) {
	ctx := context.Background()
	w, e := metering.Open(filepath.Join(t.TempDir(), "wal"), metering.Options{NodeID: "hk-01"})
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	readings := []metering.Reading{}
	for _, dir := range []string{"original", "reply"} {
		readings = append(readings, metering.Reading{Counter: metering.Counter{Name: "c_" + dir, RuleID: "00000000-0000-4000-8000-000000000001", SubscriptionID: "00000000-0000-4000-8000-000000000002", Protocol: "tcp", Direction: dir}, Bytes: 100, Packets: 1})
	}
	if _, e = w.BeginEpochFor(ctx, "kernel-one", readings); e != nil {
		t.Fatal(e)
	}
	record, _, _ := w.Next()
	c := Client{BaseURL: "https://control.example", NodeID: "hk-01"}
	c.HTTP = &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) { return nil, errors.New("lost connection") })}
	if e = c.FlushUsage(ctx, w); e == nil {
		t.Fatal("network failure acknowledged")
	}
	again, ok, _ := w.Next()
	if !ok || string(again.Payload) != string(record.Payload) {
		t.Fatal("lost WAL on network error")
	}
	c.HTTP.Transport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/agent/usage" || r.Method != "POST" {
			t.Fatal("wrong route")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != string(record.Payload) {
			t.Fatal("retry changed payload")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ruleId":"wrong","epoch":"wrong","sequence":1}`))}, nil
	})
	if e = c.FlushUsage(ctx, w); !errors.Is(e, metering.ErrAck) {
		t.Fatalf("wrong receipt discarded WAL: %v", e)
	}
	c.HTTP.Transport = usageTransport(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(metering.Receipt{RuleID: record.Report.RuleID, SubscriptionID: record.Report.SubscriptionID, Epoch: record.Report.Epoch, Sequence: record.Report.Sequence})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if e = c.FlushUsage(ctx, w); e != nil {
		t.Fatal(e)
	}
	if _, ok, _ = w.Next(); ok {
		t.Fatal("accepted report retained")
	}
}
