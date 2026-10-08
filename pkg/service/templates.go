package service

import (
	_ "embed"
)

// Embedded service templates

//go:embed templates/com.zzrouter.node.plist
var launchdPlistTemplate string

// GetLaunchdPlistTemplate returns the launchd plist template
func GetLaunchdPlistTemplate() string {
	return launchdPlistTemplate
}
