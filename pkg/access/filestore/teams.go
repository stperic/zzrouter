package filestore

import (
	"bytes"
	"fmt"

	"github.com/stperic/zzrouter/pkg/access/teams"
	"gopkg.in/yaml.v3"
)

// teamsFile is the on-disk format for teams.yaml.
type teamsFile struct {
	Version string                 `yaml:"version"`
	Teams   map[string]*teams.Team `yaml:"teams,omitempty"`
}

// DecodeTeams validates one persisted policy set without installing it.
func DecodeTeams(data []byte) (map[string]*teams.Team, error) {
	var cfg teamsFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse teams config: %w", err)
	}

	// Decode effective mappings as well: YAML null (including aliases/merges)
	// skips a value unmarshaller, but must not mean an omitted legacy policy.
	var shape struct {
		Teams map[string]map[string]any `yaml:"teams"`
	}
	if err := yaml.Unmarshal(data, &shape); err != nil {
		return nil, err
	}
	for id, fields := range shape.Teams {
		for _, field := range []string{"provider_access", "route_access"} {
			if value, present := fields[field]; present && value == nil {
				return nil, fmt.Errorf("team %q: %s must be a policy object", id, field)
			}
		}
	}

	loaded := make(map[string]*teams.Team, len(cfg.Teams))
	for id, team := range cfg.Teams {
		if team == nil {
			return nil, fmt.Errorf("team %q must be an object", id)
		}
		if err := team.Validate(); err != nil {
			return nil, fmt.Errorf("team %q: %w", id, err)
		}
		team.ID = id
		loaded[id] = team
	}

	return loaded, nil
}

func encodeTeams(records map[string]*teams.Team) ([]byte, error) {
	return yaml.Marshal(&teamsFile{Version: "1", Teams: records})
}
