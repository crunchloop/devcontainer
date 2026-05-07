package config

import "sort"

// SortedKeys returns the keys of m in lexicographic order. Useful when
// rendering map-typed fields (e.g. ContainerEnv, RemoteEnv) deterministically
// for logging or test fixtures, since Go map iteration order is unspecified.
func SortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
