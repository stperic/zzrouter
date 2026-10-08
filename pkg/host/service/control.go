package service

import (
	"context"
	"fmt"

	"github.com/stperic/zzrouter/pkg/host"
)

// Controller starts and stops an existing service without installing a host unit.
type Controller interface {
	Control(context.Context, string, string) error
}

func controlCommand(ctx context.Context, command string, args ...string) error {
	output, err := host.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("service control: %w: %.1024s", err, output)
	}
	return nil
}

func (s *Systemd) Control(ctx context.Context, name, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unsupported service action %q", action)
	}
	return controlCommand(ctx, "systemctl", action, name)
}

func (n *NSSM) Control(ctx context.Context, name, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unsupported service action %q", action)
	}
	return controlCommand(ctx, "nssm", action, name)
}

func (l *Launchd) Control(ctx context.Context, name, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unsupported service action %q", action)
	}
	if l.isBrew {
		return controlCommand(ctx, "brew", "services", action, name)
	}
	// Unloading the plist suppresses KeepAlive, unlike launchctl stop.
	if l.detectedPlist == "" {
		return fmt.Errorf("service %q has no controllable plist; host owner must register it", name)
	}
	if action == "stop" || action == "restart" {
		if err := controlCommand(ctx, "launchctl", "unload", l.detectedPlist); err != nil {
			return err
		}
	}
	if action != "stop" {
		return controlCommand(ctx, "launchctl", "load", l.detectedPlist)
	}
	return nil
}
