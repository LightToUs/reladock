package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/sourceflow"
)

func uploadEvent(now time.Time) sourceflow.Event {
	return sourceflow.Event{EventID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", NodeID: "hk-01", AllocationID: "11111111-1111-4111-8111-111111111111", ConfigGeneration: 7, RuleVersion: 3, Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, RelayIP: "203.0.113.10", RelayPort: 24443, TargetIP: "198.51.100.20", TargetPort: 443, ObservedAt: now, KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ConntrackID: 42}
}

func TestSourceFlowUploadPrunesOnlyAfterExactDurableReceipt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	w, err := sourceflow.Open(filepath.Join(t.TempDir(), "source"), sourceflow.Options{NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err = w.AppendEvent(context.Background(), uploadEvent(now)); err != nil {
		t.Fatal(err)
	}
	bad := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/agent/source-flows" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request path=%s content-type=%s", request.URL.Path, request.Header.Get("Content-Type"))
		}
		var batch sourceflow.Batch
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		receipt := batch.Receipt()
		if bad {
			receipt.EventIDs[0] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
		}
		json.NewEncoder(response).Encode(receipt)
	}))
	defer server.Close()
	client := Client{HTTP: server.Client(), BaseURL: server.URL, NodeID: "hk-01"}
	if err = client.FlushSourceFlows(context.Background(), w); err == nil {
		t.Fatal("mismatched receipt accepted")
	}
	status, _ := w.Status()
	if status.PendingEvents != 1 {
		t.Fatalf("event pruned before exact ACK: %+v", status)
	}
	bad = false
	if err = client.FlushSourceFlows(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	status, _ = w.Status()
	if status.PendingEvents != 0 {
		t.Fatalf("event not pruned after ACK: %+v", status)
	}
}
