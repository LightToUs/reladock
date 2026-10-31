package agent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestPreflightOutputCannotBypassLimitThroughReaderFrom(t *testing.T) {
	var output preflightOutput
	input := strings.Repeat("x", 16*1024)
	if _, bypass := any(&output).(io.ReaderFrom); bypass {
		t.Fatal("preflight output exposes an unbounded ReaderFrom path")
	}
	n, err := io.Copy(&output, io.LimitReader(strings.NewReader(input), int64(len(input))))
	if err != nil || n != int64(len(input)) || output.Len() != 480 {
		t.Fatalf("copied=%d retained=%d err=%v", n, output.Len(), err)
	}
}

func TestPreflightOutputStaysBoundedAfterJSONEscaping(t *testing.T) {
	var output preflightOutput
	unsafe := []byte(strings.Repeat("<>&\x01\xff\"\\\n", 100))
	if _, err := output.Write(unsafe); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(output.String())
	if err != nil || len(encoded) > 2*480+2 {
		t.Fatalf("encoded output grew beyond bound: bytes=%d err=%v", len(encoded), err)
	}
}

func TestPreflightReadOnlyAndBounded(t *testing.T) {
	report, err := InspectHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(report)
	if err != nil || len(b) > 7000 {
		t.Fatal("unbounded report")
	}
	if report.Mode != "validation-only" || report.ForwardingReady {
		t.Fatal("inspection implies forwarding readiness")
	}
	if !strings.Contains(string(b), "checks") {
		t.Fatal("no checks")
	}
}
