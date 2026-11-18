package vllm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// decodeJSONObject decodes a vLLM JSON-object flag value. vLLM accepts a JSON
// object for these flags ("Should either be a valid JSON string or JSON keys
// passed individually"); anything else is a configuration error.
func decodeJSONObject(raw string) (JSONMap, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("empty JSON object")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&m); err != nil {
		return nil, errors.New("not a valid JSON object")
	}
	if m == nil {
		return nil, errors.New("not a valid JSON object")
	}
	if dec.More() {
		return nil, errors.New("trailing content after the JSON object")
	}
	return JSONMap(m), nil
}

// jsonString returns m[key] when it is a string.
func (m JSONMap) jsonString(key string) (*string, error) {
	if m == nil {
		return nil, nil
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%q must be a string", key)
	}
	return strPtr(s), nil
}

// jsonBool returns m[key] when it is a bool.
func (m JSONMap) jsonBool(key string) (*bool, error) {
	if m == nil {
		return nil, nil
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("%q must be a boolean", key)
	}
	return &b, nil
}

// jsonInt returns m[key] when it is an integer number.
func (m JSONMap) jsonInt(key string) (*int, error) {
	if m == nil {
		return nil, nil
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.Atoi(n.String())
		if err != nil {
			return nil, fmt.Errorf("%q must be an integer", key)
		}
		return &i, nil
	case float64:
		if n != float64(int(n)) {
			return nil, fmt.Errorf("%q must be an integer", key)
		}
		i := int(n)
		return &i, nil
	default:
		return nil, fmt.Errorf("%q must be an integer", key)
	}
}

// jsonObject returns m[key] when it is a nested JSON object.
func (m JSONMap) jsonObject(key string) (JSONMap, error) {
	if m == nil {
		return nil, nil
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	switch nested := v.(type) {
	case map[string]any:
		return JSONMap(nested), nil
	case JSONMap:
		return nested, nil
	default:
		return nil, fmt.Errorf("%q must be a JSON object", key)
	}
}

// humanReadableInt parses the integers vLLM accepts, including the decimal and
// binary multipliers of vllm.utils.argparse_utils.human_readable_int: "1k" is
// 1000, "1K" is 1024, "25.6k" is 25600.
var humanReadableIntPattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)([kKmMgGtT])$`)

var decimalMultipliers = map[string]float64{
	"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12,
}

var binaryMultipliers = map[string]float64{
	"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40,
}

func humanReadableInt(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if m := humanReadableIntPattern.FindStringSubmatch(value); m != nil {
		number, suffix := m[1], m[2]
		if mult, ok := decimalMultipliers[suffix]; ok {
			f, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, err
			}
			return int(f * mult), nil
		}
		mult := binaryMultipliers[suffix]
		if strings.Contains(number, ".") {
			return 0, fmt.Errorf("decimals are not allowed with binary suffixes like %s", suffix)
		}
		i, err := strconv.Atoi(number)
		if err != nil {
			return 0, err
		}
		return int(float64(i) * mult), nil
	}
	return strconv.Atoi(value)
}

// humanReadableIntOrAuto parses an integer where "-1" and "auto" mean
// auto-detection.
func humanReadableIntOrAuto(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "-1" || strings.EqualFold(value, "auto") {
		return -1, nil
	}
	return humanReadableInt(value)
}

// digestOf returns the hex SHA-256 of s.
func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
