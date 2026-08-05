package core

import (
	"context"
	"sync"
	"time"
)

type sessionLaneKey struct {
	Platform  string
	AccountID string
	UserKey   string
	SessionID string
}

type sessionLane struct {
	token chan struct{}
	refs  int
}

type sessionLaneSet struct {
	mu    sync.Mutex
	lanes map[sessionLaneKey]*sessionLane
}

func newSessionLaneSet() *sessionLaneSet {
	return &sessionLaneSet{lanes: map[sessionLaneKey]*sessionLane{}}
}

func (s *sessionLaneSet) acquire(ctx context.Context, key sessionLaneKey) (func(), time.Duration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	lane := s.retain(key)
	select {
	case <-ctx.Done():
		s.releaseReference(key, lane)
		return nil, time.Since(started), ctx.Err()
	case <-lane.token:
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		lane.token <- struct{}{}
		s.releaseReference(key, lane)
	}, time.Since(started), nil
}

func (s *sessionLaneSet) retain(key sessionLaneKey) *sessionLane {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lanes == nil {
		s.lanes = map[sessionLaneKey]*sessionLane{}
	}
	lane := s.lanes[key]
	if lane == nil {
		lane = &sessionLane{token: make(chan struct{}, 1)}
		lane.token <- struct{}{}
		s.lanes[key] = lane
	}
	lane.refs++
	return lane
}

func (s *sessionLaneSet) releaseReference(key sessionLaneKey, lane *sessionLane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.lanes[key]
	if current != lane {
		return
	}
	lane.refs--
	if lane.refs == 0 {
		delete(s.lanes, key)
	}
}

func (b *Bot) withSessionLane(ctx context.Context, msg InboundMessage, sessionID string, run func(context.Context) error) error {
	key := sessionLaneKey{
		Platform:  msg.Platform,
		AccountID: msg.AccountID,
		UserKey:   msg.UserKey,
		SessionID: sessionID,
	}
	lanes := b.sessionLaneSet()
	release, waited, err := lanes.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	started := time.Now()
	coreLog.Debug(ctx, "session lane acquired platform=%s account=%s session=%s wait_ms=%d", key.Platform, key.AccountID, key.SessionID, waited.Milliseconds())
	defer func() {
		coreLog.Debug(ctx, "session lane released platform=%s account=%s session=%s duration_ms=%d", key.Platform, key.AccountID, key.SessionID, time.Since(started).Milliseconds())
	}()
	return run(ctx)
}

func (b *Bot) sessionLaneSet() *sessionLaneSet {
	b.laneMu.Lock()
	defer b.laneMu.Unlock()
	if b.lanes == nil {
		b.lanes = newSessionLaneSet()
	}
	return b.lanes
}
