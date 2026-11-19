package constants

// Headers that name where zzRouter served a request. On a response they
// tell the client. On a request only a coordinator sends them, to the
// worker it dispatches to, so every surface a client reaches drops them.
const (
	HeaderServingNode     = "X-zzrouter-Node"
	HeaderServingProvider = "X-zzrouter-Provider"
	// HeaderResponseOwner carries a caller-and-tenant digest only between trusted nodes.
	HeaderResponseOwner = "X-Zzrouter-Response-Owner"
)

// RoutingHintHeaders returns the request headers that steer dispatch and
// that no client may set. A fresh slice, so a caller cannot change the set.
func RoutingHintHeaders() []string {
	return []string{HeaderServingNode, HeaderServingProvider, HeaderResponseOwner}
}
