package clientcli

import (
	"bytes"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type failedOutputWriter struct{}

func (failedOutputWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestOutputDataUsesCommandWriterAndReportsFailures(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.PersistentFlags().String("output", format, "format")
			var output bytes.Buffer
			cmd.SetOut(&output)
			data := struct {
				Name string `json:"name"`
			}{Name: "example"}
			require.NoError(t, OutputData(cmd, data, nil))
			if format == "json" {
				require.JSONEq(t, `{"name":"example"}`, output.String())
			} else {
				require.Equal(t, "name: example\n", output.String())
			}
			cmd.SetOut(failedOutputWriter{})
			require.ErrorIs(t, OutputData(cmd, data, nil), io.ErrClosedPipe)
		})
	}
}
