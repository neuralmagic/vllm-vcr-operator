package translator

import (
	"encoding/json"
	"fmt"

	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// frontendArg renders a preserved setting as frontend command-line arguments.
//
// A nil result means the setting contributes nothing to the command line, which
// the policy table documents in the row's Reason (for example a credential that
// is copied verbatim at mutation time).
type frontendArg func(*vllm.Config) []string

// strArg emits "--flag value" for a string setting.
func strArg(flag string, get func(*vllm.Config) *string) frontendArg {
	return func(c *vllm.Config) []string {
		value := get(c)
		if value == nil {
			return nil
		}
		return []string{flag, *value}
	}
}

// intArg emits "--flag value" for an integer setting.
func intArg(flag string, get func(*vllm.Config) *int) frontendArg {
	return func(c *vllm.Config) []string {
		value := get(c)
		if value == nil {
			return nil
		}
		return []string{flag, fmt.Sprintf("%d", *value)}
	}
}

// boolArg emits "--flag" for true and the negated spelling "--no-<flag>" for
// false, which is how vLLM 0.33.0 spells an explicit false.
func boolArg(flag string, get func(*vllm.Config) *bool) frontendArg {
	return func(c *vllm.Config) []string {
		value := get(c)
		if value == nil {
			return nil
		}
		if *value {
			return []string{flag}
		}
		return []string{"--no-" + trimDashes(flag)}
	}
}

// listArg emits a repeated "--flag value" sequence for a string list.
func listArg(flag string, get func(*vllm.Config) []string) frontendArg {
	return func(c *vllm.Config) []string {
		values := get(c)
		if len(values) == 0 {
			return nil
		}
		out := make([]string, 0, len(values)*2)
		for _, value := range values {
			out = append(out, flag, value)
		}
		return out
	}
}

// jsonArg emits "--flag <json>" for an object setting. A non-nil empty object is
// emitted as "{}" so that an explicit empty object is not lost.
func jsonArg(flag string, get func(*vllm.Config) vllm.JSONMap) frontendArg {
	return func(c *vllm.Config) []string {
		value := get(c)
		if value == nil {
			return nil
		}
		encoded, err := json.Marshal(map[string]any(value))
		if err != nil {
			// A JSONMap was decoded from JSON, so re-encoding cannot fail; if it
			// ever does, omitting the argument is safer than emitting garbage.
			return nil
		}
		return []string{flag, string(encoded)}
	}
}

// intMapArg emits "--flag <json>" for a map-valued setting, which vLLM's flexible
// argument parser accepts in JSON form.
func intMapArg(flag string, get func(*vllm.Config) map[string]int) frontendArg {
	return func(c *vllm.Config) []string {
		value := get(c)
		if value == nil {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		return []string{flag, string(encoded)}
	}
}

// trimDashes returns the flag name without its leading dashes.
func trimDashes(flag string) string {
	for len(flag) > 0 && flag[0] == '-' {
		flag = flag[1:]
	}
	return flag
}
