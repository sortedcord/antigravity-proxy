package proxy

import (
	"context"
	"crypto/sha256"
	"time"
)

const modelCatalogTTL = 5 * time.Minute

type modelCatalogKey struct {
	account [32]byte
	project string
	epoch   uint64
}

type modelCatalogFlight struct {
	key      modelCatalogKey
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	models   []geminiModel
	err      error
	endpoint string
}

type modelCatalogCache struct {
	key      modelCatalogKey
	models   []geminiModel
	expires  time.Time
	endpoint string
	flight   *modelCatalogFlight
	clock    func() time.Time
}

func (cache *modelCatalogCache) now() time.Time {
	if cache.clock != nil {
		return cache.clock()
	}
	return time.Now()
}

func cloneGeminiModels(models []geminiModel) []geminiModel {
	copyModels := make([]geminiModel, len(models))
	copy(copyModels, models)
	for i := range copyModels {
		copyModels[i].InputTokenLimit = append([]byte(nil), models[i].InputTokenLimit...)
		copyModels[i].OutputTokenLimit = append([]byte(nil), models[i].OutputTokenLimit...)
	}
	return copyModels
}

func (p *Proxy) invalidateGeminiModelCatalog() {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	p.catalogEpoch++
	p.catalog.models = nil
	p.catalog.expires = time.Time{}
	// Existing callers may finish a healthy endpoint fallback, but stale
	// results must never repopulate the invalidated cache.
	p.catalog.flight = nil
}

// cachedGeminiModels shares one fetch per account/project. A canceled waiter
// leaves independently; upstream is canceled only when its last waiter leaves.
func (p *Proxy) cachedGeminiModels(ctx context.Context, token, project string) ([]geminiModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.catalogMu.Lock()
	key := modelCatalogKey{account: sha256.Sum256([]byte(token)), project: project, epoch: p.catalogEpoch}
	if p.catalog.key == key && p.catalog.now().Before(p.catalog.expires) {
		models, endpoint := cloneGeminiModels(p.catalog.models), p.catalog.endpoint
		p.catalogMu.Unlock()
		recordRequestEndpoint(ctx, endpoint)
		return models, nil
	}
	flight := p.catalog.flight
	if flight == nil || flight.key != key {
		// Do not tie a shared fetch to the first request's cancellation or
		// access state. The explicit waiter count owns its cancellation.
		// A different account/project must not cancel existing callers.
		fetchCtx, cancel := context.WithCancel(context.Background())
		fetchCtx = context.WithValue(fetchCtx, accessStateKey{}, &accessRequestState{})
		flight = &modelCatalogFlight{key: key, done: make(chan struct{}), cancel: cancel}
		p.catalog.flight = flight
		go p.fetchModelCatalogFlight(fetchCtx, token, project, flight)
	}
	flight.waiters++
	p.catalogMu.Unlock()
	select {
	case <-ctx.Done():
		p.catalogMu.Lock()
		flight.waiters--
		if flight.waiters == 0 {
			if p.catalog.flight == flight {
				p.catalog.flight = nil
			}
			flight.cancel()
		}
		p.catalogMu.Unlock()
		return nil, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recordRequestEndpoint(ctx, flight.endpoint)
		return cloneGeminiModels(flight.models), flight.err
	}
}

func (p *Proxy) fetchModelCatalogFlight(ctx context.Context, token, project string, flight *modelCatalogFlight) {
	defer flight.cancel()
	models, err := p.fetchGeminiModels(ctx, token, project)
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	flight.models, flight.err, flight.endpoint = models, err, requestEndpoint(ctx)
	if p.catalog.flight == flight {
		p.catalog.flight = nil
		if err == nil && ctx.Err() == nil && p.catalogEpoch == flight.key.epoch {
			p.catalog.key, p.catalog.models, p.catalog.endpoint = flight.key, models, flight.endpoint
			p.catalog.expires = p.catalog.now().Add(modelCatalogTTL)
		}
	}
	close(flight.done)
}
