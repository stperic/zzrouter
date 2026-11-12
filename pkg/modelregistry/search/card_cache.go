package search

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/stperic/zzrouter/pkg/config/backend"
)

const modelCardCacheMaxSize = 500

// CardCache owns bounded model descriptions, partitioned by credential revision.
// Returned values are copies so callers cannot mutate retained cache data.
type CardCache struct {
	mu    sync.RWMutex
	cards map[string]*ModelCard
}

// NewCardCache constructs a service-owned card cache.
func NewCardCache() *CardCache { return &CardCache{cards: make(map[string]*ModelCard)} }

func cloneCard(card *ModelCard) *ModelCard {
	if card == nil {
		return nil
	}
	copy := *card
	copy.Tags = slices.Clone(card.Tags)
	copy.Siblings = slices.Clone(card.Siblings)
	return &copy
}

// GetModelCard fetches a card with the operation's immutable connection snapshot.
func (c *CardCache) GetModelCard(ctx context.Context, provider Provider, modelID string, target *backend.Resolved) (*ModelCard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	revision := ""
	if target != nil {
		revision = target.Revision
	}
	key := fmt.Sprintf("%s:%s:%s", provider, revision, modelID)
	c.mu.RLock()
	cached := cloneCard(c.cards[key])
	c.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	var card *ModelCard
	var err error
	switch provider {
	case ProviderHuggingFace:
		if target == nil {
			return nil, fmt.Errorf("hugging face connection is unavailable")
		}
		card, err = NewClient(ClientOptions{Target: target}).getHuggingFaceModelCard(ctx, modelID)
	case ProviderOllama:
		card, err = getOllamaModelCard(ctx, modelID)
	default:
		card, err = getCloudModelCard(ctx, provider, modelID, target)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.cards) >= modelCardCacheMaxSize {
		clear(c.cards)
	}
	c.cards[key] = cloneCard(card)
	c.mu.Unlock()
	return card, nil
}
