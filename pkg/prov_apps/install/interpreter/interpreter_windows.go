//go:build windows

package interpreter

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"

	"github.com/stperic/zzrouter/pkg/config"
)

// defaultCandidates includes registered service-visible interpreters before PATH aliases.
func defaultCandidates() []string {
	candidates := []string{}
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		key, err := registry.OpenKey(root, `Software\Python`, registry.READ|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		candidates = append(candidates, registeredCandidates(key)...)
		_ = key.Close()
	}
	return append(candidates, "python3.14.exe", "python3.13.exe", "python3.12.exe", "python3.11.exe", "python3.10.exe", "python3.9.exe", "python3.exe", "python.exe")
}

// registeredCandidates reads PEP 514 paths only, never launcher arguments or other users' hives.
func registeredCandidates(root registry.Key) []string {
	const keyLimit = 128
	companies, err := root.ReadSubKeyNames(keyLimit + 1)
	if err != nil && len(companies) == 0 || len(companies) > keyLimit {
		return nil
	}
	slices.Sort(companies)
	candidates := []string{}
	examined := 0
	for _, company := range companies {
		key, err := registry.OpenKey(root, company, registry.READ|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		tags, readErr := key.ReadSubKeyNames(keyLimit + 1)
		_ = key.Close()
		if readErr != nil && len(tags) == 0 || len(tags) > keyLimit {
			continue
		}
		slices.Sort(tags)
		for _, tag := range tags {
			if examined >= keyLimit {
				return candidates
			}
			examined++
			key, err := registry.OpenKey(root, company+`\`+tag+`\InstallPath`, registry.QUERY_VALUE|registry.WOW64_64KEY)
			if err != nil {
				continue
			}
			path, valueErr := registryString(key, "ExecutablePath")
			if valueErr != nil {
				if directory, err := registryString(key, ""); err == nil {
					path = filepath.Join(directory, "python.exe")
				}
			}
			_ = key.Close()
			if len(path) <= 32768 && filepath.IsAbs(path) {
				candidates = append(candidates, path)
			}
		}
	}
	return candidates
}

func registryString(key registry.Key, name string) (string, error) {
	data := make([]byte, 2*32768)
	n, kind, err := key.GetValue(name, data)
	if err != nil {
		return "", err
	}
	if n > len(data) || n%2 != 0 || kind != registry.SZ && kind != registry.EXPAND_SZ {
		return "", fmt.Errorf("invalid Python registry path")
	}
	units := make([]uint16, n/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	if len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	return string(utf16.Decode(units)), nil
}

func versionProbeArgs() []string {
	platform := map[string]string{"amd64": "win-amd64", "arm64": "win-arm64", "386": "win32"}[runtime.GOARCH]
	return []string{"-I", "-S", "-c", "import sys,sysconfig;sys.exit(1) if sysconfig.get_platform()!=sys.argv[1] else print('Python '+sys.version.split()[0])", platform}
}

func preferredInstallTarget(req *config.PythonRequirement) string {
	if req == nil || req.Max == "" {
		return "3.13"
	}
	m, ok := parseMinorTuple(req.Max)
	if !ok || m.minor <= 1 {
		return "3.13"
	}
	suggest := m.minor - 1
	if suggest < 9 {
		suggest = 9
	}
	return fmt.Sprintf("%d.%d", m.major, suggest)
}

func installHintForTarget(target string) string {
	return fmt.Sprintf(
		"Install Python %s from python.org or via winget:\n"+
			"  winget install Python.Python.%s\n"+
			"Use a machine-wide installation for a Windows service; registered interpreters are discovered without PATH changes.",
		target, target,
	)
}
