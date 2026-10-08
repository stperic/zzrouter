//go:build !linux && !darwin && !windows

package service

// NewServiceManager returns a basic fallback manager for unsupported platforms
func NewServiceManager() ServiceManager {
	return &BasicManager{}
}
