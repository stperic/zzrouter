package mesh

// Scheme returns "https" when the admin port has TLS enabled and
// "http" otherwise. Used to build cluster endpoint URLs from a
// host:port pair when the admin port's scheme is the right source
// (e.g., registry URLs for broadcast/dispatch). The cluster mTLS
// port uses its own hardcoded https regardless — see
// clusternode.CoordinatorSelfCoordURL.
func Scheme(tlsEnabled bool) string {
	if tlsEnabled {
		return "https"
	}
	return "http"
}
