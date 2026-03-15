package agent

import (
	"fmt"

	"dario.cat/mergo"
	"gopkg.in/yaml.v3"
)

// parseYAML parses a YAML string into a map.
func parseYAML(yamlStr string) (map[string]interface{}, error) {
	var result map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlStr), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}
	return result, nil
}

// toYAML converts a map to a YAML string.
func toYAML(data map[string]interface{}) (string, error) {
	yamlBytes, err := yaml.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal to YAML: %w", err)
	}
	return string(yamlBytes), nil
}

// mergeConfigs merges src into dst, with src taking precedence.
// Uses the mergo library for deep merging.
func mergeConfigs(dst, src map[string]interface{}) map[string]interface{} {
	// Create a copy of dst to avoid modifying the original
	result := make(map[string]interface{})
	for k, v := range dst {
		result[k] = v
	}

	// Merge src into result with override
	if err := mergo.Merge(&result, src, mergo.WithOverride); err != nil {
		// If merge fails, just return dst
		return dst
	}

	return result
}
