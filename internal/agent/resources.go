package agent

import (
	"context"
	"errors"
	"math/bits"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"reladock/internal/platform"
)

type DataplaneInspector interface {
	Stable(context.Context) error
}

type filesystemUsage struct {
	Blocks    uint64
	Available uint64
}

type cpuCounters struct {
	total uint64
	idle  uint64
}

type ResourceCollector struct {
	mu             sync.Mutex
	previousCPU    *cpuCounters
	now            func() time.Time
	readFile       func(string) ([]byte, error)
	statFS         func(string) (filesystemUsage, error)
	inspectTimeout time.Duration
}

func NewResourceCollector() *ResourceCollector {
	return &ResourceCollector{
		now:      time.Now,
		readFile: os.ReadFile,
		statFS: func(path string) (filesystemUsage, error) {
			var value syscall.Statfs_t
			if err := syscall.Statfs(path, &value); err != nil {
				return filesystemUsage{}, err
			}
			return filesystemUsage{Blocks: value.Blocks, Available: value.Bavail}, nil
		},
		inspectTimeout: 2 * time.Second,
	}
}

func boundedPercent(used, total uint64) *int {
	if total == 0 || used > total {
		return nil
	}
	high, low := bits.Mul64(used, 100)
	quotient, _ := bits.Div64(high, low, total)
	value := int(quotient)
	return &value
}

func parseCPU(raw []byte) (cpuCounters, bool) {
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, false
	}
	var result cpuCounters
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil || ^uint64(0)-result.total < value {
			return cpuCounters{}, false
		}
		result.total += value
		if i == 3 || i == 4 {
			if ^uint64(0)-result.idle < value {
				return cpuCounters{}, false
			}
			result.idle += value
		}
	}
	return result, true
}

func parseMemory(raw []byte) *int {
	var total, available uint64
	var totalSeen, availableSeen bool
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value
			totalSeen = true
		case "MemAvailable:":
			available = value
			availableSeen = true
		}
	}
	if !totalSeen || !availableSeen || available > total {
		return nil
	}
	return boundedPercent(total-available, total)
}

func parseSingleUint(raw []byte) (uint64, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return value, err == nil
}

func (c *ResourceCollector) cpuPercent() *int {
	raw, err := c.readFile("/proc/stat")
	if err != nil {
		return nil
	}
	current, ok := parseCPU(raw)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.previousCPU
	c.previousCPU = &current
	if previous == nil || current.total <= previous.total || current.idle < previous.idle {
		return nil
	}
	total := current.total - previous.total
	idle := current.idle - previous.idle
	if idle > total {
		return nil
	}
	return boundedPercent(total-idle, total)
}

func (c *ResourceCollector) dataplaneState(ctx context.Context, inspector DataplaneInspector) string {
	if inspector == nil {
		return "not-applicable"
	}
	timeout := c.inspectTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	check, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := inspector.Stable(check)
	if err == nil && check.Err() == nil {
		return "in-sync"
	}
	if check.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "unknown"
	}
	return "drifted"
}

func (c *ResourceCollector) Collect(ctx context.Context, inspector DataplaneInspector) platform.ResourceObservation {
	if c == nil {
		return platform.ResourceObservation{SampledAt: time.Now().UTC(), DataplaneState: "unknown"}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.readFile == nil {
		c.readFile = os.ReadFile
	}
	if c.statFS == nil {
		c.statFS = NewResourceCollector().statFS
	}
	value := platform.ResourceObservation{
		SampledAt:      c.now().UTC().Truncate(time.Microsecond),
		CPUPercent:     c.cpuPercent(),
		DataplaneState: c.dataplaneState(ctx, inspector),
	}
	if raw, err := c.readFile("/proc/meminfo"); err == nil {
		value.MemoryPercent = parseMemory(raw)
	}
	if usage, err := c.statFS("/"); err == nil && usage.Available <= usage.Blocks {
		value.DiskPercent = boundedPercent(usage.Blocks-usage.Available, usage.Blocks)
	}
	countRaw, countErr := c.readFile("/proc/sys/net/netfilter/nf_conntrack_count")
	maxRaw, maxErr := c.readFile("/proc/sys/net/netfilter/nf_conntrack_max")
	if countErr == nil && maxErr == nil {
		count, countOK := parseSingleUint(countRaw)
		maximum, maxOK := parseSingleUint(maxRaw)
		if countOK && maxOK && count <= maximum {
			value.ConntrackPercent = boundedPercent(count, maximum)
		}
	}
	return value
}
