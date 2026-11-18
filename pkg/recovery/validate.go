package recovery

import (
	"crypto/tls"
	"fmt"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/filestore"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/config"
)

func validateContent(name string) bool {
	switch name {
	case "config/node.yaml", "config/keys.yaml", "config/teams.yaml", "config/spend.json", "config/access/people.json", "config/oauth.json", "config/credentials.enc", "config/identity/node.key", "config/identity/node.pem", "config/ca/ca.key", "config/ca/ca.pem":
		return true
	}
	return false
}

func validateSnapshots(files map[string][]byte, key []byte) error {
	if _, err := config.DecodeNodeConfig(files["config/node.yaml"]); err != nil {
		return fmt.Errorf("node configuration: %w", err)
	}
	teamRecords := make(map[string]*teams.Team)
	if data, ok := files["config/teams.yaml"]; ok {
		var err error
		teamRecords, err = filestore.DecodeTeams(data)
		if err != nil {
			return fmt.Errorf("team snapshot: %w", err)
		}
	}
	if data, ok := files["config/keys.yaml"]; ok {
		records, err := filestore.DecodeKeys(data)
		if err != nil {
			return fmt.Errorf("key snapshot: %w", err)
		}
		for id, record := range records {
			if teamRecords[record.TeamID] == nil {
				return fmt.Errorf("key %s references a missing team", id)
			}
		}
	}
	if data, ok := files["config/access/people.json"]; ok {
		records, err := people.DecodeSnapshot(data)
		if err != nil {
			return fmt.Errorf("people snapshot: %w", err)
		}
		for _, person := range records {
			if teamRecords[person.TeamID] == nil {
				return fmt.Errorf("person %s references a missing team", person.ID)
			}
		}
	}
	if data, ok := files["config/spend.json"]; ok {
		if _, err := quota.DecodeSpend(data); err != nil {
			return fmt.Errorf("spend snapshot: %w", err)
		}
	}
	if data, ok := files["config/oauth.json"]; ok {
		if err := oauth.ValidateState[control.CredentialReference](data); err != nil {
			return fmt.Errorf("OAuth snapshot: %w", err)
		}
	}
	if data, ok := files["config/credentials.enc"]; ok {
		if err := secrets.ValidateState(data, key); err != nil {
			return fmt.Errorf("protected connection snapshot: %w", err)
		}
	}
	for _, pair := range [][2]string{{"config/identity/node.pem", "config/identity/node.key"}, {"config/ca/ca.pem", "config/ca/ca.key"}} {
		cert, certExists := files[pair[0]]
		private, keyExists := files[pair[1]]
		if certExists != keyExists {
			return fmt.Errorf("incomplete trust pair: %s", pair[0])
		}
		if certExists {
			if _, err := tls.X509KeyPair(cert, private); err != nil {
				return fmt.Errorf("invalid trust pair %s: %w", pair[0], err)
			}
		}
	}
	return nil
}
