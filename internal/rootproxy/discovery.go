package rootproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// relayCatalogRefreshInterval bounds how stale the routing half of the Relay
	// catalog can become while Root is idle. Discovery also runs opportunistically
	// on every model-catalog request, so this only matters for a Root that serves
	// inference without Desktop ever re-reading the catalog.
	relayCatalogRefreshInterval = 5 * time.Minute
	// maxRelayCatalogBytes bounds the loopback Relay catalog response.
	maxRelayCatalogBytes = 8 << 20
)

// discoveredRelayModel is one entry of the Relay catalog reduced to the two
// fields Root routes on.
type discoveredRelayModel struct {
	id       string
	provider relayProvider
}

// relayCatalogResponse is the OpenAI-shaped listing served by Relay /v1/models.
type relayCatalogResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// relayDiscovery fetches the Relay model catalog and keeps the shared route
// resolver in sync with it.
type relayDiscovery struct {
	baseURL  string
	apiKey   string
	client   *http.Client
	resolver *routeResolver

	mu        sync.Mutex
	succeeded bool

	startOnce sync.Once
}

func newRelayDiscovery(baseURL, apiKey string, client *http.Client, resolver *routeResolver) (*relayDiscovery, error) {
	validated, errURL := validateHTTPBaseURL(baseURL, "relay")
	if errURL != nil {
		return nil, errURL
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("relay API key is empty")
	}
	if client == nil {
		return nil, errors.New("relay discovery requires an HTTP client")
	}
	if resolver == nil {
		return nil, errors.New("relay discovery requires a route resolver")
	}
	return &relayDiscovery{
		baseURL:  validated,
		apiKey:   apiKey,
		client:   client,
		resolver: resolver,
	}, nil
}

// start runs an immediate refresh and then keeps the catalog current on a
// ticker. Safe to call more than once; only one loop runs.
func (d *relayDiscovery) start(ctx context.Context) {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		go d.run(ctx)
	})
}

func (d *relayDiscovery) run(ctx context.Context) {
	d.refreshLogged(ctx, "startup relay catalog discovery")
	ticker := time.NewTicker(relayCatalogRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshLogged(ctx, "periodic relay catalog discovery")
		}
	}
}

func (d *relayDiscovery) refreshLogged(ctx context.Context, label string) {
	models, err := d.refresh(ctx)
	if err != nil {
		// Keep the last good catalog rather than emptying the Relay half, which
		// would silently reroute every Relay model to the official arm.
		log.Warnf("root proxy: %s failed, keeping current catalog: %v", label, err)
		return
	}
	log.Infof("root proxy: %s found %d relay model(s)", label, len(models))
}

// refresh fetches the Relay catalog and swaps it into the resolver.
func (d *relayDiscovery) refresh(ctx context.Context) ([]discoveredRelayModel, error) {
	if d == nil {
		return nil, errors.New("relay discovery is unavailable")
	}
	models, errFetch := d.fetch(ctx)
	if errFetch != nil {
		return nil, errFetch
	}
	accepted, collisions := d.resolver.applyRelayCatalog(models)
	for _, model := range collisions {
		log.Warnf("root proxy: relay model %q collides with a pinned stock model and stays on the official route", model)
	}
	d.mu.Lock()
	d.succeeded = true
	d.mu.Unlock()
	log.Debugf("root proxy: relay catalog accepted %d model(s): %s", len(accepted), strings.Join(accepted, ", "))
	return models, nil
}

// ensure performs a blocking refresh when discovery has never succeeded, so a
// Relay that was unreachable at startup does not cause its models to be routed
// to the official arm.
func (d *relayDiscovery) ensure(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	done := d.succeeded
	d.mu.Unlock()
	if done {
		return
	}
	if _, err := d.refresh(ctx); err != nil {
		log.Warnf("root proxy: relay catalog has never been discovered; relay models are not routable yet: %v", err)
	}
}

// fetch reads the Relay catalog. It installs no deadline of its own: the caller's
// context is the only bound, matching Root's no-timeout transport policy.
func (d *relayDiscovery) fetch(ctx context.Context) ([]discoveredRelayModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/models", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("build relay catalog request: %w", errRequest)
	}
	request.Header.Set("Authorization", "Bearer "+d.apiKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set(rootHopHeader, "1")

	response, errDo := d.client.Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("fetch relay catalog: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debugf("root proxy: close relay catalog body error: %v", errClose)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay catalog returned HTTP %d", response.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxRelayCatalogBytes))
	if errRead != nil {
		return nil, fmt.Errorf("read relay catalog: %w", errRead)
	}
	return parseRelayCatalog(body)
}

func parseRelayCatalog(body []byte) ([]discoveredRelayModel, error) {
	var payload relayCatalogResponse
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode relay catalog: %w", errDecode)
	}
	models := make([]discoveredRelayModel, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || id != entry.ID {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		model := discoveredRelayModel{id: id}
		// An unrecognised owned_by leaves the model routable but unattributed,
		// which means it cannot create or consume compaction.
		if provider, errProvider := parseRelayProvider(strings.TrimSpace(entry.OwnedBy)); errProvider == nil {
			model.provider = provider
		} else if strings.TrimSpace(entry.OwnedBy) != "" {
			log.Debugf("root proxy: relay model %q has unmapped owned_by %q; compaction stays disabled for it", id, entry.OwnedBy)
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("relay catalog contains no usable models")
	}
	// Deterministic order keeps the merged catalog and its ETag stable when the
	// Relay listing order changes without the set changing.
	sort.Slice(models, func(i, j int) bool { return models[i].id < models[j].id })
	return models, nil
}
