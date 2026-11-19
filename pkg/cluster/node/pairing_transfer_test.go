package clusternode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPairingTransferRoundTripAndRefusals(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	valid := PairingWindowInfo{NodeName: "gpu-computer", Code: "ABCDEFGHIJKLMNOP", WorkerURL: "https://worker.example:9091", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Deadline: now.Add(time.Minute)}.Transfer()
	data, err := json.Marshal(valid)
	require.NoError(t, err)
	got, err := ReadPairingTransfer(strings.NewReader(string(data)), now)
	require.NoError(t, err)
	require.Equal(t, valid, got)
	for name, change := range map[string]func(*PairingTransfer){
		"version":     func(p *PairingTransfer) { p.Version = 2 },
		"type":        func(p *PairingTransfer) { p.Type = "shell" },
		"code":        func(p *PairingTransfer) { p.Code = "bad" },
		"expired":     func(p *PairingTransfer) { p.ExpiresAt = now },
		"name":        func(p *PairingTransfer) { p.NodeName = "x\nspoof" },
		"HTTP":        func(p *PairingTransfer) { p.WorkerURL = "http://worker.example" },
		"userinfo":    func(p *PairingTransfer) { p.WorkerURL = "https://secret@worker.example" },
		"query":       func(p *PairingTransfer) { p.WorkerURL += "?code=secret" },
		"fingerprint": func(p *PairingTransfer) { p.WorkerFingerprint = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			change(&p)
			require.Error(t, p.Validate(now))
		})
	}
	for _, malformed := range []string{string(data) + "{}", strings.Repeat(" ", MaxPairingTransferSize+1), strings.Replace(string(data), `"version":1`, `"extra":true,"version":1`, 1), "null"} {
		_, err := ReadPairingTransfer(strings.NewReader(malformed), now)
		require.Error(t, err)
	}
}
