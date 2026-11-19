//go:build windows

package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

// CheckInstallationCredentialOwner refuses an ACL rewrite that would exclude
// an existing daemon account. Call before stopping or modifying an installation.
func CheckInstallationCredentialOwner(configDir string) error {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("inspect service credential ownership: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	name, err := windows.UTF16PtrFromString(windowsServiceName)
	if err != nil {
		return err
	}
	handle, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing service account before changing credentials: %w", err)
	}
	svc := &mgr.Service{Name: windowsServiceName, Handle: handle}
	defer func() { _ = svc.Close() }()
	settings, err := svc.Config()
	if err != nil {
		return fmt.Errorf("read existing service account: %w", err)
	}
	if err := checkWindowsServiceConfigDir(settings.BinaryPathName, configDir); err != nil {
		return err
	}
	if settings.ServiceStartName == "" || settings.ServiceStartName == "LocalSystem" || settings.ServiceStartName == `NT AUTHORITY\SYSTEM` {
		return nil
	}
	account, _, _, err := windows.LookupSID("", settings.ServiceStartName)
	if err != nil {
		return fmt.Errorf("resolve the existing service account before changing credentials: %w", err)
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("inspect installer account: %w", err)
	}
	if account.Equals(current.User.Sid) {
		return nil
	}
	return fmt.Errorf("the existing service uses a custom account; run its upgrade under that account or preserve its filesystem permissions through an administrator-managed upgrade")
}

func checkWindowsServiceConfigDir(commandLine, requested string) error {
	args, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return fmt.Errorf("inspect the existing service command: %w", err)
	}
	var installed string
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--dir" || args[i] == "-C":
			if i+1 == len(args) || installed != "" {
				return fmt.Errorf("the existing service has ambiguous config-directory arguments")
			}
			i++
			installed = args[i]
		case strings.HasPrefix(args[i], "--dir="):
			if installed != "" {
				return fmt.Errorf("the existing service has repeated config-directory arguments")
			}
			installed = strings.TrimPrefix(args[i], "--dir=")
		}
	}
	if installed == "" {
		// Earlier service definitions used environment discovery. Read only the
		// selected config-directory value, never infer it from the installer user.
		serviceKey, openErr := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\zzrouter`, registry.QUERY_VALUE)
		if openErr != nil {
			return fmt.Errorf("inspect existing service directory: %w", openErr)
		}
		values, _, readErr := serviceKey.GetStringsValue("Environment")
		_ = serviceKey.Close()
		if readErr != nil && !errors.Is(readErr, registry.ErrNotExist) {
			return fmt.Errorf("inspect service environment directory: %w", readErr)
		}
		for _, entry := range values {
			name, value, _ := strings.Cut(entry, "=")
			if strings.EqualFold(name, "ZZROUTER_CONFIG_DIR") {
				installed = value
			}
		}
		if installed == "" {
			machineKey, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE)
			if err != nil {
				return fmt.Errorf("inspect machine config-directory setting: %w", err)
			}
			installed, _, readErr = machineKey.GetStringValue("ZZROUTER_CONFIG_DIR")
			_ = machineKey.Close()
			if readErr != nil && !errors.Is(readErr, registry.ErrNotExist) {
				return fmt.Errorf("read machine config-directory setting: %w", readErr)
			}
		}
	}
	if installed == "" || !filepath.IsAbs(installed) || !strings.EqualFold(filepath.Clean(installed), filepath.Clean(requested)) {
		return fmt.Errorf("the requested config directory does not match the existing service; use its explicit --dir or machine config-directory setting before upgrading")
	}
	return nil
}
