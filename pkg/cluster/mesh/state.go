package mesh

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// StateManager manages all cluster state in a unified way
type StateManager struct {
	endpoints       *EndpointRegistry
	circuitBreakers *CircuitBreakerManager
	downloadTracker *DownloadTracker
}

// NewStateManager creates a new unified state manager
func NewStateManager(endpoints *EndpointRegistry, circuitBreakers *CircuitBreakerManager) *StateManager {
	return &StateManager{
		endpoints:       endpoints,
		circuitBreakers: circuitBreakers,
		downloadTracker: NewDownloadTracker(),
	}
}

// GetEndpoints returns the endpoint registry
func (sm *StateManager) GetEndpoints() *EndpointRegistry {
	return sm.endpoints
}

// GetCircuitBreakers returns the circuit breaker manager
func (sm *StateManager) GetCircuitBreakers() *CircuitBreakerManager {
	return sm.circuitBreakers
}

// GetDownloadTracker returns the download tracker
func (sm *StateManager) GetDownloadTracker() *DownloadTracker {
	return sm.downloadTracker
}

// NOTE: ModelCache removed - using unified cache in server.go instead
// Model caching is now handled at the server level, not in the cluster package

// DownloadTracker tracks downloads across all cluster hosts
type DownloadTracker struct {
	downloads map[string]*Download // key: host/model
	mu        sync.RWMutex
}

// Download represents a model download
type Download struct {
	Key        string    // Unique key: host/repo/model
	Node       string    // Node performing download
	Registry   string    // Registry (ollama, huggingface)
	Model      string    // Model name
	Status     string    // Status: downloading, completed, failed
	Progress   float64   // Progress percentage (0-100)
	Downloaded int64     // Bytes downloaded
	TotalSize  int64     // Total size in bytes
	Speed      int64     // Download speed (bytes/sec)
	Message    string    // Status message
	StartTime  time.Time // When download started
	UpdateTime time.Time // Last update time
}

// NewDownloadTracker creates a new download tracker
func NewDownloadTracker() *DownloadTracker {
	return &DownloadTracker{
		downloads: make(map[string]*Download),
	}
}

// StartDownload registers a new download
func (dt *DownloadTracker) StartDownload(key, host, repo, model string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.downloads[key] = &Download{
		Key:        key,
		Node:       host,
		Registry:   repo,
		Model:      model,
		Status:     string(constants.StatusDownloading),
		StartTime:  utils.Now(),
		UpdateTime: utils.Now(),
	}
}

// GetDownload retrieves a specific download
func (dt *DownloadTracker) GetDownload(key string) (*Download, bool) {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	download, exists := dt.downloads[key]
	return download, exists
}

// GetAllDownloads returns all downloads
func (dt *DownloadTracker) GetAllDownloads() []*Download {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	downloads := make([]*Download, 0, len(dt.downloads))
	for _, download := range dt.downloads {
		downloads = append(downloads, download)
	}
	return downloads
}

// GetDownloadsByNode returns downloads for a specific host
func (dt *DownloadTracker) GetDownloadsByNode(host string) []*Download {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	downloads := make([]*Download, 0)
	for _, download := range dt.downloads {
		if download.Node == host {
			downloads = append(downloads, download)
		}
	}
	return downloads
}

// RemoveDownload removes a download from tracking
func (dt *DownloadTracker) RemoveDownload(key string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	delete(dt.downloads, key)
}

// CleanCompleted removes completed/failed downloads
func (dt *DownloadTracker) CleanCompleted() int {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	cleaned := 0
	for key, download := range dt.downloads {
		if download.Status == string(constants.StatusCompleted) || download.Status == string(constants.StatusFailed) {
			delete(dt.downloads, key)
			cleaned++
		}
	}
	return cleaned
}

// CleanOld removes downloads older than specified duration
func (dt *DownloadTracker) CleanOld(maxAge time.Duration) int {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	cleaned := 0
	now := utils.Now()
	for key, download := range dt.downloads {
		if now.Sub(download.UpdateTime) > maxAge {
			delete(dt.downloads, key)
			cleaned++
		}
	}
	return cleaned
}
