package pythonvenv

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"fmt"
)

// relocateWindowsLauncher changes only a distlib launcher's appended interpreter line.
func relocateWindowsLauncher(data []byte, candidate, live string) ([]byte, error) {
	file, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	defer file.Close()
	var overlay uint64
	for _, section := range file.Sections {
		end := uint64(section.Offset) + uint64(section.Size)
		if end > overlay {
			overlay = end
		}
	}
	if overlay == 0 || overlay >= uint64(len(data)) {
		return data, nil
	}
	tail := data[overlay:]
	lineEnd := bytes.IndexByte(tail, '\n')
	if lineEnd < 0 || !bytes.HasPrefix(tail, []byte("#!")) || !bytes.HasPrefix(tail[lineEnd+1:], []byte("PK\x03\x04")) {
		return data, nil
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) != 1 || archive.File[0].Name != "__main__.py" {
		return nil, fmt.Errorf("invalid Python console launcher archive")
	}
	line := bytes.ReplaceAll(tail[:lineEnd], []byte(candidate), []byte(live))
	if bytes.Equal(line, tail[:lineEnd]) {
		return data, nil
	}
	result := append(bytes.Clone(data[:overlay]), line...)
	return append(result, tail[lineEnd:]...), nil
}
