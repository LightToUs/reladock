package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type resourceInspector func(context.Context) error

func (f resourceInspector) Stable(ctx context.Context) error { return f(ctx) }

func intValue(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

func TestResourceCollectorUsesKernelCountersAndCPUDeltas(t *testing.T) {
	stat := []string{
		"cpu  50 0 25 25 0 0 0 0 0 0\n",
		"cpu  110 0 45 45 0 0 0 0 0 0\n",
	}
	collector := &ResourceCollector{
		now: func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) },
		readFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/stat":
				value := stat[0]
				stat = stat[1:]
				return []byte(value), nil
			case "/proc/meminfo":
				return []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\n"), nil
			case "/proc/sys/net/netfilter/nf_conntrack_count":
				return []byte("80\n"), nil
			case "/proc/sys/net/netfilter/nf_conntrack_max":
				return []byte("100\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		statFS: func(string) (filesystemUsage, error) {
			return filesystemUsage{Blocks: 1000, Available: 100}, nil
		},
	}

	first := collector.Collect(context.Background(), nil)
	if first.CPUPercent != nil {
		t.Fatalf("first CPU sample must remain unknown: %+v", first)
	}
	second := collector.Collect(context.Background(), nil)
	if second.SampledAt.IsZero() || intValue(second.CPUPercent) != 80 || intValue(second.MemoryPercent) != 75 || intValue(second.DiskPercent) != 90 || intValue(second.ConntrackPercent) != 80 || second.DataplaneState != "not-applicable" {
		t.Fatalf("unexpected resource sample: %+v", second)
	}
}

func TestResourceCollectorKeepsUnavailableCountersUnknown(t *testing.T) {
	collector := &ResourceCollector{
		now:      func() time.Time { return time.Now().UTC() },
		readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		statFS:   func(string) (filesystemUsage, error) { return filesystemUsage{}, os.ErrPermission },
	}
	value := collector.Collect(context.Background(), nil)
	if value.CPUPercent != nil || value.MemoryPercent != nil || value.DiskPercent != nil || value.ConntrackPercent != nil {
		t.Fatalf("unavailable counters were converted into values: %+v", value)
	}
}

func TestResourceCollectorClassifiesReadOnlyDataplaneVerification(t *testing.T) {
	collector := &ResourceCollector{
		now:            func() time.Time { return time.Now().UTC() },
		readFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
		statFS:         func(string) (filesystemUsage, error) { return filesystemUsage{}, os.ErrNotExist },
		inspectTimeout: 10 * time.Millisecond,
	}
	tests := []struct {
		name      string
		inspector DataplaneInspector
		want      string
	}{
		{"validation mode", nil, "not-applicable"},
		{"verified", resourceInspector(func(context.Context) error { return nil }), "in-sync"},
		{"mismatch", resourceInspector(func(context.Context) error { return errors.New("readback mismatch") }), "drifted"},
		{"timeout", resourceInspector(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := collector.Collect(context.Background(), tt.inspector)
			if value.DataplaneState != tt.want {
				t.Fatalf("dataplane state=%q want %q", value.DataplaneState, tt.want)
			}
		})
	}
}

func TestResourceCollectorRejectsCounterRegressionAndMalformedFiles(t *testing.T) {
	stat := []string{"cpu  90 0 0 10\n", "cpu  80 0 0 10\n"}
	collector := &ResourceCollector{
		now: func() time.Time { return time.Now().UTC() },
		readFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/stat":
				value := stat[0]
				stat = stat[1:]
				return []byte(value), nil
			case "/proc/meminfo":
				return []byte("MemTotal: 0 kB\nMemAvailable: 9 kB\n"), nil
			case "/proc/sys/net/netfilter/nf_conntrack_count", "/proc/sys/net/netfilter/nf_conntrack_max":
				return []byte("not-a-number"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		statFS: func(string) (filesystemUsage, error) { return filesystemUsage{Blocks: 10, Available: 11}, nil },
	}
	_ = collector.Collect(context.Background(), nil)
	value := collector.Collect(context.Background(), nil)
	if value.CPUPercent != nil || value.MemoryPercent != nil || value.DiskPercent != nil || value.ConntrackPercent != nil {
		t.Fatalf("malformed/regressed counters were accepted: %+v", value)
	}
}
