package monitor

import "sync"

// backgroundTaskGroup tracks callback work that must finish before tests or
// callers tear down the dependencies used by that work. Reserve lets a
// callback secure a lifecycle slot before it consumes durable one-shot state.
type backgroundTaskGroup struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
}

func (g *backgroundTaskGroup) Reserve() (release func(), ok bool) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, false
	}
	g.wg.Add(1)
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(g.wg.Done)
	}, true
}

func (g *backgroundTaskGroup) Go(run func()) bool {
	if run == nil {
		return false
	}
	release, ok := g.Reserve()
	if !ok {
		return false
	}
	go func() {
		defer release()
		run()
	}()
	return true
}

func (g *backgroundTaskGroup) CloseAdmission() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *backgroundTaskGroup) Wait() {
	g.wg.Wait()
}

func (g *backgroundTaskGroup) CloseAndWait() {
	g.CloseAdmission()
	g.Wait()
}
