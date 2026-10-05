package dockerclient

import (
	"cmp"
	"slices"
)

// sortedKeys returns the map keys in a stable order.
//
// Docker returns networks as a map, whose iteration order Go deliberately
// randomises. Publishing records in that order would make the hosts file
// change on every reconcile even when nothing happened, which would defeat
// the idempotency check and churn the file for no reason.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, cmp.Compare)
	return keys
}
