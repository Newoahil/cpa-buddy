package config

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// ModelProfile binds a user-facing configuration name to a provider and,
// optionally, exact quota-window names or labels.
type ModelProfile struct {
	Name     string              `json:"name"`
	Provider domain.ProviderKind `json:"provider"`
	Windows  []string            `json:"windows,omitempty"`
}

// ParseModelProfiles rejects invalid mappings rather than silently using none.
// Errors identify the setting and entry, never echo its potentially private data.
func ParseModelProfiles(raw string) ([]ModelProfile, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var profiles []ModelProfile
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profiles); err != nil {
		return nil, fmt.Errorf("parse MODEL_PROFILES_JSON: invalid JSON array: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("MODEL_PROFILES_JSON must contain one array")
	}
	if profiles == nil {
		return nil, fmt.Errorf("MODEL_PROFILES_JSON must be an array")
	}
	seen := make(map[string]bool, len(profiles))
	for i := range profiles {
		p := &profiles[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			return nil, fmt.Errorf("MODEL_PROFILES_JSON[%d]: name is required", i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("MODEL_PROFILES_JSON[%d]: duplicate name", i)
		}
		seen[p.Name] = true
		switch p.Provider {
		case domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI, domain.ProviderOllama:
		default:
			return nil, fmt.Errorf("MODEL_PROFILES_JSON[%d]: unknown provider", i)
		}
		for j, w := range p.Windows {
			if strings.TrimSpace(w) == "" {
				return nil, fmt.Errorf("MODEL_PROFILES_JSON[%d].windows[%d]: empty window", i, j)
			}
		}
	}
	return profiles, nil
}
