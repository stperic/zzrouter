package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// StartToken reads an opaque, exec-stable identity, compared exactly on this platform.
func StartToken(pid int) (int64, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseStartTokenStat(string(stat))
}

func parseStartTokenStat(stat string) (int64, error) {
	// comm can contain spaces and parentheses; field 3 starts after the last ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("process stat has no comm terminator")
	}
	fields := strings.Fields(stat[end+1:])
	const startTokenIndex = 22 - 3
	if len(fields) <= startTokenIndex {
		return 0, fmt.Errorf("process stat has no start token")
	}
	token, err := strconv.ParseInt(fields[startTokenIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start token: %w", err)
	}
	if token <= 0 {
		return 0, fmt.Errorf("process start token must be positive")
	}
	return token, nil
}
