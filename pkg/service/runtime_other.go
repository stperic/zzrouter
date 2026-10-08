//go:build !windows

package service

func runUnderServiceManager(_ func() error) (bool, error) { return false, nil }
