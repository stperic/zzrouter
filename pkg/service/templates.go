package service

import (
	_ "embed"
)

// Embedded service templates

//
//go:embed templates/zzrouter-node.service
//nolint:unused // referenced only by systemd.go (build-tag: linux)
var systemdServiceTemplate string

// The privileged updater: a root oneshot, the path unit that lets the
// unprivileged node trigger it, and the timer that runs the scheduled
// check. See templates/zzrouter-update.service for why it is split.

//go:embed templates/zzrouter-update.service
//nolint:unused // referenced only by systemd.go (build-tag: linux)
var systemdUpdateServiceTemplate string

//go:embed templates/zzrouter-update.path
//nolint:unused // referenced only by systemd.go (build-tag: linux)
var systemdUpdatePathTemplate string

//go:embed templates/zzrouter-update.timer
//nolint:unused // referenced only by systemd.go (build-tag: linux)
var systemdUpdateTimerTemplate string

//go:embed templates/com.zzrouter.node.plist
var launchdPlistTemplate string

// GetLaunchdPlistTemplate returns the launchd plist template
func GetLaunchdPlistTemplate() string {
	return launchdPlistTemplate
}
