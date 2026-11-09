package pythonvenv

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsConsoleLauncherRelocationPreservesPEAndZip(t *testing.T) {
	// Synthetic PE section layout plus a real appended ZIP matches distlib's framing.
	stub := make([]byte, 1024)
	copy(stub, "MZ")
	binary.LittleEndian.PutUint32(stub[0x3c:], 128)
	copy(stub[128:], "PE\x00\x00")
	var header bytes.Buffer
	require.NoError(t, binary.Write(&header, binary.LittleEndian, pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_AMD64, NumberOfSections: 1}))
	require.NoError(t, binary.Write(&header, binary.LittleEndian, pe.SectionHeader32{SizeOfRawData: 512, PointerToRawData: 512}))
	copy(stub[132:], header.Bytes())
	var payload bytes.Buffer
	archive := zip.NewWriter(&payload)
	entry, err := archive.Create("__main__.py")
	require.NoError(t, err)
	_, err = entry.Write([]byte("print('console entrypoint')\n"))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	candidate, live := `C:\candidate é\venv`, `C:\managed\venv`
	line := `#!"` + candidate + `\Scripts\python.exe"` + "\n"
	input := append(bytes.Clone(stub), []byte(line)...)
	input = append(input, payload.Bytes()...)
	result, err := relocateWindowsLauncher(input, candidate, live)
	require.NoError(t, err)
	require.Equal(t, stub, result[:len(stub)])
	require.Contains(t, string(result), `#!"`+live+`\Scripts\python.exe"`+"\n")
	require.NotContains(t, string(result), candidate)
	require.True(t, bytes.HasSuffix(result, payload.Bytes()), "ZIP bytes must remain exact")
	decoded, err := zip.NewReader(bytes.NewReader(result), int64(len(result)))
	require.NoError(t, err)
	reader, err := decoded.File[0].Open()
	require.NoError(t, err)
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "print('console entrypoint')\n", string(body))
	for _, data := range [][]byte{stub, []byte("unrelated binary\x00"), append(bytes.Clone(stub), []byte("unrelated overlay")...)} {
		unchanged, err := relocateWindowsLauncher(data, candidate, live)
		require.NoError(t, err)
		require.Equal(t, data, unchanged)
	}
	broken := append(bytes.Clone(stub), []byte(line+"PK\x03\x04broken")...)
	_, err = relocateWindowsLauncher(broken, candidate, live)
	require.ErrorContains(t, err, "invalid Python console launcher")
}
