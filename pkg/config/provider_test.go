package config

// Interface conformance — compile-time guard that every typed provider
// satisfies the Provider interface. Delete one of these lines and the
// package stops compiling, surfacing interface drift immediately.

var (
	_ Provider = (*OnDemandProvider)(nil)
	_ Provider = (*ExternalProvider)(nil)
	_ Provider = (*CloudProvider)(nil)
	_ Provider = (*SearchRegistry)(nil)
)
