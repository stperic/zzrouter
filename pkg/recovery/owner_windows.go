package recovery

// RequireOwner checks nothing on Windows, where the service runs as
// LocalSystem.
func RequireOwner(string, string) error { return nil }
