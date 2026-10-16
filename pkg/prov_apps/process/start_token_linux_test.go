package process

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStartTokenStat(t *testing.T) {
	// Number the surrounding fields so off-by-one parsing cannot pass.
	var fields []string
	for field := 4; field <= 24; field++ {
		fields = append(fields, strconv.Itoa(field))
	}
	for _, comm := range []string{"engine", "a) b (c"} {
		t.Run(comm, func(t *testing.T) {
			token, err := parseStartTokenStat("123 (" + comm + ") S " + strings.Join(fields, " ") + "\n")
			require.NoError(t, err)
			assert.EqualValues(t, 22, token)
		})
	}
	for _, stat := range []string{
		"", "123 engine S", "123 (engine) S 4", "123 (engine)",
		"123 (engine) S " + strings.Join(fields[:18], " ") + " bad",
		"123 (engine) S " + strings.Join(fields[:18], " ") + " 0",
		"123 (engine) S " + strings.Join(fields[:18], " ") + " -1",
	} {
		_, err := parseStartTokenStat(stat)
		assert.Error(t, err, "%q", stat)
	}
}

func TestStartTokenIsRawProcTicks(t *testing.T) {
	pid, token, _, _ := identityChild(t)
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	require.NoError(t, err)
	// The fixture's comm has no spaces, so field 22 can be read independently.
	fields := strings.Fields(string(data))
	want, err := strconv.ParseInt(fields[21], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, want, token, "the token must not include a reconstructed boot time")
}
