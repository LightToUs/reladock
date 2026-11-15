//go:build !linux

package sourceflow

import "context"

type unsupportedEventSource struct{}

func NewConntrackEventSource() EventSource { return unsupportedEventSource{} }

func (unsupportedEventSource) Run(context.Context, func(), func(KernelEvent) bool) error {
	return ErrUnsupported
}
