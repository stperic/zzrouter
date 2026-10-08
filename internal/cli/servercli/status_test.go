package servercli

import (
	"testing"
)

func TestCheckNodeRunning(t *testing.T) {
	// Test with a port that's unlikely to be in use
	// This should return false since no server is running
	result := checkNodeRunning(59999)
	if result {
		t.Error("checkNodeRunning(59999) should return false for unused port")
	}
}
