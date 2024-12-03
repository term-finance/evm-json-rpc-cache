package api

import (
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

type URLManager struct {
	urls        []string
	currentIdx  atomic.Uint32
	mu          sync.RWMutex
	logger      *zap.SugaredLogger
	name        string
	lastSuccess atomic.Bool
}

func NewURLManager(urls []string, logger *zap.SugaredLogger, name string) *URLManager {
	if len(urls) == 0 {
		panic("no URLs provided to URLManager")
	}
	manager := &URLManager{
		urls:   urls,
		logger: logger,
		name:   name,
	}
	manager.lastSuccess.Store(true)

	logger.Infow("Initialized URL manager",
		"manager", name,
		"initial_url", urls[0],
	)
	return manager
}

func (m *URLManager) GetCurrentURL() string {
	current := m.currentIdx.Load()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.urls[current%uint32(len(m.urls))]
}

func (m *URLManager) MarkSuccess() {
	m.lastSuccess.Store(true)
}

func (m *URLManager) MarkFailure() string {
	m.lastSuccess.Store(false)
	next := m.currentIdx.Add(1)
	m.mu.RLock()
	nextURL := m.urls[next%uint32(len(m.urls))]
	m.mu.RUnlock()

	m.logger.Infow("Switching to next URL due to failure",
		"manager", m.name,
		"url", nextURL,
	)
	return nextURL
}

func (m *URLManager) UpdateURLs(urls []string) {
	if len(urls) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.urls = urls
}
