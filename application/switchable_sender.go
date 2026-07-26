package application

import (
	"context"
	"sync"

	"go.klarlabs.de/briefkasten/domain"
)

// SwitchableSender is a Sender whose transport can be swapped at runtime
// (runtime reconfiguration), the outbound counterpart of Switchable. The outbox
// worker holds one instance for its lifetime; Swap atomically repoints it at a
// freshly-built sender, so a credentials or provider change takes effect on the
// next delivery without restarting the worker.
type SwitchableSender struct {
	mu     sync.RWMutex
	sender domain.Sender
}

// NewSwitchableSender wraps an initial sender.
func NewSwitchableSender(s domain.Sender) *SwitchableSender {
	return &SwitchableSender{sender: s}
}

// Swap replaces the sender for all subsequent deliveries.
func (s *SwitchableSender) Swap(next domain.Sender) {
	s.mu.Lock()
	s.sender = next
	s.mu.Unlock()
}

// Send delivers via the current sender.
func (s *SwitchableSender) Send(ctx context.Context, msg domain.OutboundMessage) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	return sender.Send(ctx, msg)
}

// From reports the current sender's address, so a reply derived while a
// swap is pending excludes the address the message will actually leave
// from rather than one that has already been replaced.
func (s *SwitchableSender) From() string {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if a, ok := sender.(domain.SelfAddresser); ok {
		return a.From()
	}
	return ""
}

var (
	_ domain.Sender        = (*SwitchableSender)(nil)
	_ domain.SelfAddresser = (*SwitchableSender)(nil)
)
