package views

import (
	"errors"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// writeOutcomeUnknown requires a fresh read before retrying an ambiguous mutation.
func writeOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *pkgClient.APIError
	return !errors.As(err, &apiErr) || apiErr.StatusCode >= 500
}
