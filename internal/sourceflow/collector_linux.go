//go:build linux

package sourceflow

import (
	"context"
	"errors"
	"syscall"
	"time"

	conntrack "github.com/florianl/go-conntrack"
)

type conntrackEventSource struct{}

func NewConntrackEventSource() EventSource { return conntrackEventSource{} }

func (conntrackEventSource) Run(ctx context.Context, ready func(), emit func(KernelEvent) bool) error {
	nfct, err := conntrack.Open(&conntrack.Config{AddConntrackInformation: true})
	if err != nil {
		return err
	}
	defer nfct.Close()
	errChan := nfct.AttachErrChan()
	// A zero mask matches every IPv4 source while still requiring the IPv4
	// original-tuple attribute, excluding IPv6 in the socket filter.
	filter := []conntrack.ConnAttr{{Type: conntrack.AttrOrigIPv4Src, Data: []byte{0, 0, 0, 0}, Mask: []byte{0, 0, 0, 0}}}
	if err = nfct.RegisterFiltered(ctx, conntrack.Conntrack, conntrack.NetlinkCtNew, filter, func(connection conntrack.Con) int {
		if event, ok := kernelEventFromConntrack(connection, time.Now().UTC()); ok {
			emit(event)
		}
		return 0
	}); err != nil {
		return err
	}
	ready()
	select {
	case <-ctx.Done():
		return nil
	case err, ok := <-errChan:
		if !ok || err == nil {
			return errors.New("conntrack event channel closed")
		}
		if errors.Is(err, syscall.ENOBUFS) {
			return errors.Join(ErrNetlinkOverrun, err)
		}
		return err
	}
}

func kernelEventFromConntrack(connection conntrack.Con, receivedAt time.Time) (KernelEvent, bool) {
	if connection.Origin == nil || connection.Origin.Src == nil || connection.Origin.Dst == nil || connection.Origin.Proto == nil || connection.Origin.Proto.Number == nil || connection.Origin.Proto.SrcPort == nil || connection.Origin.Proto.DstPort == nil || connection.ID == nil {
		return KernelEvent{}, false
	}
	protocol := ""
	switch *connection.Origin.Proto.Number {
	case 6:
		protocol = "tcp"
	case 17:
		protocol = "udp"
	default:
		return KernelEvent{}, false
	}
	zone := uint16(0)
	if connection.Zone != nil {
		zone = *connection.Zone
	} else if connection.Origin.Zone != nil {
		zone = *connection.Origin.Zone
	}
	event := KernelEvent{
		Protocol: protocol, SourceIP: connection.Origin.Src.String(), SourcePort: int(*connection.Origin.Proto.SrcPort),
		DestinationIP: connection.Origin.Dst.String(), DestinationPort: int(*connection.Origin.Proto.DstPort),
		ObservedAt: receivedAt.UTC().Truncate(time.Microsecond), Zone: zone, ID: *connection.ID,
	}
	_, valid := kernelEventKey("00000000-0000-4000-8000-000000000000", event)
	return event, valid
}
