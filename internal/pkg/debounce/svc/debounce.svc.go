// Package svc: per-chat debounce -- wait for a message burst to settle
// before calling the LLM. A person typing sends 3-5 messages in a row;
// answering each one burns tokens for no reason. Trigger (re)schedules the
// callback DebounceSec after the latest message, but never lets it slip
// past DebounceMaxSec from the first message of the burst.
package svc

import (
	"sync"
	"time"
)

type Service struct {
	debounce    time.Duration
	debounceMax time.Duration

	mu         sync.Mutex
	timers     map[int64]*time.Timer
	batchStart map[int64]time.Time
}

func New(debounceSec, debounceMaxSec int) *Service {
	return &Service{
		debounce:    time.Duration(debounceSec) * time.Second,
		debounceMax: time.Duration(debounceMaxSec) * time.Second,
		timers:      make(map[int64]*time.Timer),
		batchStart:  make(map[int64]time.Time),
	}
}

func (s *Service) Trigger(chatID int64, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	batchStart, ok := s.batchStart[chatID]
	if !ok {
		batchStart = now
		s.batchStart[chatID] = now
	}

	if t, ok := s.timers[chatID]; ok {
		t.Stop()
	}

	deadline := now.Add(s.debounce)
	if maxDeadline := batchStart.Add(s.debounceMax); deadline.After(maxDeadline) {
		deadline = maxDeadline
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}

	s.timers[chatID] = time.AfterFunc(delay, func() {
		s.mu.Lock()
		delete(s.timers, chatID)
		delete(s.batchStart, chatID)
		s.mu.Unlock()
		callback()
	})
}

// TriggerAfter schedules callback to fire exactly `delay` from now for
// chatID, replacing any pending timer for that chat (debounce or a prior
// retry) -- unlike Trigger, it ignores the burst-window (batchStart) logic
// entirely. Used to retry a chat once a rate limit is expected to have
// cleared, so an exchange that got rate-limited doesn't just go silent
// forever waiting for the customer to send one more message: whatever they
// sent while waiting is still picked up as one batch when the retry fires,
// via the normal history-based batching in llmsvc.BuildHistoryText.
func (s *Service) TriggerAfter(chatID int64, delay time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.timers[chatID]; ok {
		t.Stop()
	}
	delete(s.batchStart, chatID)

	s.timers[chatID] = time.AfterFunc(delay, func() {
		s.mu.Lock()
		delete(s.timers, chatID)
		delete(s.batchStart, chatID)
		s.mu.Unlock()
		callback()
	})
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.timers {
		t.Stop()
	}
}
