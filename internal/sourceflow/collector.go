package sourceflow

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	ErrNetlinkOverrun = errors.New("conntrack netlink overrun")
	ErrUnsupported    = errors.New("source-flow collection is unsupported on this platform")
)

// KernelEvent is the metadata-only original conntrack tuple observed before
// RelaDock's SNAT. It deliberately contains no payload or application data.
type KernelEvent struct {
	Protocol        string
	SourceIP        string
	SourcePort      int
	DestinationIP   string
	DestinationPort int
	ObservedAt      time.Time
	Zone            uint16
	ID              uint32
}

// EventSource owns one conntrack NEW subscription. emit must never be blocked;
// false means the collector queue was full and the event was accounted as a gap.
type EventSource interface {
	Run(context.Context, func(), func(KernelEvent) bool) error
}

type EventWAL interface {
	AppendEvent(context.Context, Event) (Event, error)
	OpenGap(context.Context, string, time.Time, int64) (Gap, error)
	CloseGap(context.Context, time.Time) error
	SetCollectionEnabled(context.Context, bool) error
}

type ManifestProvider interface {
	SourceFlowManifest() (Manifest, string, error)
}

type Collector struct {
	Source       EventSource
	WAL          EventWAL
	Manifest     ManifestProvider
	KernelBootID string
	QueueSize    int
	Now          func() time.Time
}

func (c *Collector) Run(ctx context.Context) error {
	if c.Source == nil || c.WAL == nil || c.Manifest == nil || !uuidPattern.MatchString(c.KernelBootID) {
		return errors.New("invalid source-flow collector configuration")
	}
	queueSize := c.QueueSize
	if queueSize == 0 {
		queueSize = 1024
	}
	if queueSize < 1 || queueSize > 65536 {
		return errors.New("invalid source-flow collector queue size")
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	events := make(chan KernelEvent, queueSize)
	ready := make(chan struct{}, 1)
	lossWake := make(chan struct{}, 1)
	var queueLoss atomic.Int64
	emit := func(event KernelEvent) bool {
		select {
		case events <- event:
			return true
		default:
			queueLoss.Add(1)
			select {
			case lossWake <- struct{}{}:
			default:
			}
			return false
		}
	}
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- c.Source.Run(ctx, func() {
			select {
			case ready <- struct{}{}:
			default:
			}
		}, emit)
	}()

	closeCleanly := func() error {
		when := now().UTC()
		persistCtx := context.WithoutCancel(ctx)
		manifest, digest, manifestErr := c.Manifest.SourceFlowManifest()
		hasEnabledRules := manifestErr == nil && ValidateManifest(manifest, digest) == nil && len(manifest.Rules) > 0
		if err := c.WAL.SetCollectionEnabled(persistCtx, false); err != nil {
			return err
		}
		if hasEnabledRules {
			_, err := c.WAL.OpenGap(persistCtx, "collector-restart", when, 0)
			return err
		}
		return c.WAL.CloseGap(persistCtx, when)
	}
	select {
	case <-ctx.Done():
		return closeCleanly()
	case err := <-sourceDone:
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return closeCleanly()
		}
		if err == nil {
			err = errors.New("conntrack event source stopped")
		}
		reason := "collector-error"
		if errors.Is(err, ErrNetlinkOverrun) {
			reason = "netlink-overrun"
		}
		persistCtx := context.WithoutCancel(ctx)
		disableErr := c.WAL.SetCollectionEnabled(persistCtx, false)
		_, gapErr := c.WAL.OpenGap(persistCtx, reason, now().UTC(), 1)
		return errors.Join(err, disableErr, gapErr)
	case <-ready:
		if err := c.WAL.SetCollectionEnabled(ctx, true); err != nil {
			return err
		}
		if err := c.WAL.CloseGap(ctx, now().UTC()); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			if err := closeCleanly(); err != nil {
				return err
			}
			return nil
		case <-lossWake:
			lost := queueLoss.Swap(0)
			if lost > 0 {
				if _, err := c.WAL.OpenGap(ctx, "queue-full", now().UTC(), lost); err != nil {
					return err
				}
			}
		case err := <-sourceDone:
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				if closeErr := closeCleanly(); closeErr != nil {
					return closeErr
				}
				return nil
			}
			if err == nil {
				err = errors.New("conntrack event source stopped")
			}
			reason := "collector-error"
			if errors.Is(err, ErrNetlinkOverrun) {
				reason = "netlink-overrun"
			}
			persistCtx := context.WithoutCancel(ctx)
			disableErr := c.WAL.SetCollectionEnabled(persistCtx, false)
			_, gapErr := c.WAL.OpenGap(persistCtx, reason, now().UTC(), 1)
			return errors.Join(err, disableErr, gapErr)
		case event := <-events:
			if ctx.Err() != nil {
				return closeCleanly()
			}
			_, valid := kernelEventKey(c.KernelBootID, event)
			if !valid {
				continue
			}
			matched, err := c.attribute(event)
			if err != nil {
				continue
			}
			if matched == nil {
				continue
			}
			matched.KernelBootID = c.KernelBootID
			matched.ConntrackZone = event.Zone
			matched.ConntrackID = event.ID
			if _, err = c.WAL.AppendEvent(ctx, *matched); err != nil {
				reason := "wal-unavailable"
				if errors.Is(err, ErrFull) {
					reason = "wal-full"
				}
				if _, gapErr := c.WAL.OpenGap(ctx, reason, now().UTC(), 1); gapErr != nil {
					return errors.Join(err, gapErr)
				}
				if !errors.Is(err, ErrFull) {
					return err
				}
				continue
			}
			if err = c.WAL.CloseGap(ctx, now().UTC()); err != nil {
				return err
			}
		}
	}
}

func kernelEventKey(bootID string, event KernelEvent) (string, bool) {
	source, sourceErr := netip.ParseAddr(event.SourceIP)
	destination, destinationErr := netip.ParseAddr(event.DestinationIP)
	event.ObservedAt = event.ObservedAt.UTC().Truncate(time.Microsecond)
	if sourceErr != nil || !source.Is4() || source.IsUnspecified() || destinationErr != nil || !destination.Is4() || !destination.IsGlobalUnicast() || destination.IsLoopback() || (event.Protocol != "tcp" && event.Protocol != "udp") || event.SourcePort < 1 || event.SourcePort > 65535 || event.DestinationPort < 1 || event.DestinationPort > 65535 || event.ID == 0 || event.ObservedAt.IsZero() {
		return "", false
	}
	return bootID + "/" + strconv.Itoa(int(event.Zone)) + "/" + strconv.FormatUint(uint64(event.ID), 10) + "/" + event.ObservedAt.Format(time.RFC3339Nano), true
}

func (c *Collector) attribute(event KernelEvent) (*Event, error) {
	manifest, digest, err := c.Manifest.SourceFlowManifest()
	if err != nil {
		return nil, err
	}
	if err = ValidateManifest(manifest, digest); err != nil {
		return nil, err
	}
	for _, rule := range manifest.Rules {
		if rule.Protocol != event.Protocol || rule.RelayIP != event.DestinationIP || rule.RelayPort != event.DestinationPort {
			continue
		}
		return &Event{
			TenantID: rule.TenantID, RuleID: rule.RuleID, NodeID: manifest.NodeID,
			AllocationID: rule.AllocationID, ConfigGeneration: manifest.Generation, RuleVersion: rule.RuleVersion,
			Protocol: rule.Protocol, SourceIP: event.SourceIP, SourcePort: event.SourcePort,
			RelayIP: rule.RelayIP, RelayPort: rule.RelayPort, TargetIP: rule.TargetIP, TargetPort: rule.TargetPort,
			ObservedAt: event.ObservedAt.UTC().Truncate(time.Microsecond),
		}, nil
	}
	return nil, nil
}

func (event KernelEvent) String() string {
	return fmt.Sprintf("%s/%s:%d>%s:%d", event.Protocol, event.SourceIP, event.SourcePort, event.DestinationIP, event.DestinationPort)
}
