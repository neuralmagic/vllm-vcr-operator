package vllm

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// configFileEntry is one top-level key of a --config file.
type configFileEntry struct {
	key   string
	value any
}

// decodeYAMLMapping decodes the YAML (or JSON) config file named by --config.
//
// vLLM 0.33.0 loads this file with yaml.safe_load and requires a mapping of long
// flag names to values. The operator also accepts JSON because YAML is a superset
// of it and the file is usually generated; anything else is rejected.
func decodeYAMLMapping(content []byte) ([]configFileEntry, *Rejection) {
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, &Rejection{Setting: string(SettingConfigFile), Reason: "the configuration file is not valid YAML or JSON"}
	}
	mapping, ok := doc.(map[string]any)
	if !ok {
		return nil, &Rejection{Setting: string(SettingConfigFile), Reason: "the configuration file is not a mapping of flag names to values"}
	}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	// The order of a YAML mapping is not significant; sorting keeps resolution
	// deterministic and independent of map iteration order.
	sort.Strings(keys)
	entries := make([]configFileEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, configFileEntry{key: key, value: mapping[key]})
	}
	return entries, nil
}

// encodeJSONObject renders a JSON object as vLLM expects it on the command line.
func encodeJSONObject(m JSONMap) (string, error) {
	encoded, err := json.Marshal(map[string]any(m))
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// parseJSONScalar decodes a dotted JSON argument value the way vLLM does: JSON
// first, then a human-readable integer, then the raw string.
func parseJSONScalar(raw string) (any, bool) {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		if i, err := humanReadableInt(raw); err == nil {
			return i, true
		}
		return nil, false
	}
	return v, true
}

// formatNumber renders a YAML number without a trailing ".0" for integers, like
// Python's str() of an int.
func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
