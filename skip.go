package sidecar

import (
	"errors"
	"fmt"
	"sync"
)

var errSkipTrack = errors.New("skip current track")

type channelRuntime struct {
	mu      sync.Mutex
	streams map[chan struct{}]struct{}
}

func (p *LocalProvider) runtimeFor(id string) *channelRuntime {
	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	if runtime := p.runtimes[id]; runtime != nil {
		return runtime
	}
	runtime := &channelRuntime{streams: make(map[chan struct{}]struct{})}
	p.runtimes[id] = runtime
	return runtime
}

func (r *channelRuntime) subscribe() chan struct{} {
	signal := make(chan struct{}, 1)
	r.mu.Lock()
	r.streams[signal] = struct{}{}
	r.mu.Unlock()
	return signal
}

func (r *channelRuntime) unsubscribe(signal chan struct{}) {
	r.mu.Lock()
	delete(r.streams, signal)
	r.mu.Unlock()
}

func (r *channelRuntime) skip() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for signal := range r.streams {
		select {
		case signal <- struct{}{}:
		default: // Keep one pending skip per active stream.
		}
	}
	return len(r.streams)
}

// Skip requests the active stream connections for this channel to advance.
func (p *LocalProvider) Skip(id string) (int, error) {
	if _, ok := p.channel(id); !ok {
		return 0, fmt.Errorf("channel %q not found", id)
	}
	p.runtimeMu.Lock()
	runtime := p.runtimes[id]
	p.runtimeMu.Unlock()
	if runtime == nil {
		return 0, nil
	}
	return runtime.skip(), nil
}
