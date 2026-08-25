package diff

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// DiffProviderQuota reports top-level provider quota-window definition changes.
func DiffProviderQuota(oldQuota, newQuota map[string]config.ProviderQuota) []string {
	keys := make(map[string]struct{}, len(oldQuota)+len(newQuota))
	for key := range oldQuota {
		keys[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
	}
	for key := range newQuota {
		keys[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	changes := make([]string, 0)
	for _, key := range ordered {
		oldValue, oldOK := lookupProviderQuota(oldQuota, key)
		newValue, newOK := lookupProviderQuota(newQuota, key)
		switch {
		case !oldOK:
			changes = append(changes, fmt.Sprintf("provider quota added: %s", key))
		case !newOK:
			changes = append(changes, fmt.Sprintf("provider quota removed: %s", key))
		case !reflect.DeepEqual(oldValue, newValue):
			changes = append(changes, fmt.Sprintf("provider quota updated: %s", key))
		}
	}
	return changes
}

func lookupProviderQuota(values map[string]config.ProviderQuota, key string) (config.ProviderQuota, bool) {
	for candidate, value := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return value, true
		}
	}
	return config.ProviderQuota{}, false
}
