package recovery

import (
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/config"
)

func validateContent(name string) bool {
	switch name {
	case "config/node.yaml", "config/spend.json", "config/oauth.json", "config/credentials.enc", "config/identity/node.key", "config/identity/node.pem", "config/ca/ca.key", "config/ca/ca.pem", "config/" + sqlstore.MarkerName:
		return true
	}
	return false
}

// validateSnapshots checks the files validateContent names; store describes
// the archive's database image, nil without one.
func validateSnapshots(files map[string][]byte, key []byte, store *StoreMeta) error {
	if data, ok := files["config/node.yaml"]; ok {
		if _, err := config.DecodeNodeConfig(data); err != nil {
			return fmt.Errorf("node configuration: %w", err)
		}
	}
	// A marker without its image would stop the restored node as missing, and
	// one naming another store would not describe the image it travels with.
	if data, ok := files["config/"+sqlstore.MarkerName]; ok {
		id, err := sqlstore.MarkerStoreID(data)
		if err == nil && (store == nil || id != store.StoreID) {
			err = errors.New("it does not name the archive's database image")
		}
		if err != nil {
			return fmt.Errorf("store marker: %w", err)
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
