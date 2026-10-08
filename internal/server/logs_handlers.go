// Logs handlers - instance log access and streaming
// Cluster-aware: queries local and remote hosts for instance logs

package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// LogsHandlers groups log access and streaming handlers.
type LogsHandlers struct {
	node                *NodeIdentity
	appMgr              *prov_apps.ProviderAppManager
	httpClient          *http.Client
	httpStreamingClient *http.Client
	getClusterClient    func() mesh.ClusterClient
	routeToClusterNode  func(ctx context.Context, node, path, method string, body []byte, headers http.Header) (*mesh.Response, error)
	getClusterNodeURL   func(string) (string, error)
	isLocalNode         func(string) bool
	handleClusterAction func(*gin.Context, string, string, string, []byte) bool
}

// HandleGetRunLogsPublic implements the RunLogHandler interface.
func (l *LogsHandlers) HandleGetRunLogsPublic(c *gin.Context) {
	l.handleGetInstanceLogs(c)
}

// handleGetInstanceLogs returns logs for a specific run.
func (l *LogsHandlers) handleGetInstanceLogs(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{RequiredPathParam("id")})
	if params == nil {
		return
	}
	instanceID := params.GetString("id")

	linesParams := ValidateParams(c, []ParamRule{
		IntQueryParam("lines", 100, 1, 10000),
		BoolQueryParam("follow"),
		IntQueryParam("context", 0, 0, logFilterMaxContext),
		IntQueryParam("max_matches", logFilterDefaultMaxMatches, 1, logFilterAbsoluteMaxMatches),
	})
	if linesParams == nil {
		return
	}
	lines := linesParams.GetInt("lines")
	follow := linesParams.GetBool("follow")
	contextLines := linesParams.GetInt("context")
	maxMatches := linesParams.GetInt("max_matches")

	filter, err := compileLogFilter(c.Query("grep"), c.Query("regex"), contextLines, maxMatches)
	if err != nil {
		BadRequest(c, err.Error())
		return
	}

	// Try local first
	if l.appMgr != nil {
		if inst, exists := l.appMgr.GetInstance(instanceID); exists {
			logFilePath := inst.LogFilePath

			if logFilePath == "" {
				ctx, cancel := context.WithTimeout(c.Request.Context(), constants.DiscoveryTimeout)
				defer cancel()

				ticker := time.NewTicker(constants.LogStreamPollInterval)
				defer ticker.Stop()

				for logFilePath == "" {
					select {
					case <-ctx.Done():
						NotFound(c, "No log file: Log file not created within timeout period")
						return
					case <-ticker.C:
						if updatedInstance, exists := l.appMgr.GetInstance(instanceID); exists {
							logFilePath = updatedInstance.LogFilePath
						}
					}
				}
			}

			if logFilePath == "" {
				NotFound(c, "No log file: Instance does not have a log file configured")
				return
			}

			ctx, cancel := context.WithTimeout(c.Request.Context(), constants.HealthCheckTimeout)
			defer cancel()

			ticker := time.NewTicker(constants.SSEPollInterval)
			defer ticker.Stop()

		WaitForFile:
			for {
				select {
				case <-ctx.Done():
					break WaitForFile
				case <-ticker.C:
					if _, err := os.Stat(logFilePath); err == nil {
						break WaitForFile
					}
				}
			}

			if follow {
				l.streamInstanceLogs(c, logFilePath, filter)
			} else {
				l.getInstanceLogLines(c, logFilePath, lines, filter)
			}
			return
		}
	}

	// Not local - find on cluster and route
	instanceData := l.findInstanceOnCluster(instanceID)
	if instanceData == nil {
		NotFound(c, fmt.Sprintf("Instance not found: Instance %s not found", instanceID))
		return
	}

	host, ok := instanceData["node"].(string)
	if !ok || l.isLocalNode(host) {
		NotFound(c, "Instance not found: Instance node information missing")
		return
	}

	if follow {
		l.proxySSELogsFromCluster(c, host, instanceID, lines)
	} else {
		l.routeInstanceLogsToCluster(c, host, instanceID, lines, follow)
	}
}

// getInstanceLogLines returns the last N lines from a log file.
func (l *LogsHandlers) getInstanceLogLines(c *gin.Context, logPath string, lines int, filter *logFilter) {
	logManager := l.appMgr.LogManager()
	if logManager != nil {
		baseDir := logManager.GetBaseDir()

		realLogPath, err1 := filepath.EvalSymlinks(logPath)
		realBaseDir, err2 := filepath.EvalSymlinks(baseDir)

		if err1 != nil {
			realLogPath, err1 = filepath.Abs(logPath)
		}
		if err2 != nil {
			realBaseDir, err2 = filepath.Abs(baseDir)
		}

		if err1 == nil && err2 == nil {
			if !strings.HasPrefix(realLogPath, realBaseDir) {
				Forbidden(c, "Access denied: Log file path outside allowed directory")
				return
			}
		}
	}

	file, err := os.Open(logPath)
	if err != nil {
		InternalNodeError(c, "Failed to open log file: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	fileInfo, err := file.Stat()
	if err != nil {
		InternalNodeError(c, "Failed to get file info: "+err.Error())
		return
	}

	const maxFilterBytes = 100 * 1024 * 1024
	if filter != nil && fileInfo.Size() > maxFilterBytes {
		RespondWithProblemOpts(c, http.StatusRequestEntityTooLarge, "Request Entity Too Large",
			fmt.Sprintf("log file is %d bytes; grep refused above %d bytes (use a stricter filter or a narrower time window once supported)",
				fileInfo.Size(), maxFilterBytes),
			ProblemOpts{Code: "log_file_too_large"})
		return
	}

	const maxFileSize = 10 * 1024 * 1024
	if fileInfo.Size() > maxFileSize && filter == nil {
		resultLines, truncated, err := readLastNLinesEfficient(file, lines)
		if err != nil {
			InternalNodeError(c, "Failed to read log file: "+err.Error())
			return
		}

		respondSuccess(c, "Logs retrieved", gin.H{
			"log_file":    logPath,
			"total_lines": len(resultLines),
			"lines":       resultLines,
			"count":       len(resultLines),
			"truncated":   truncated,
		})
		return
	}

	var logLines []string
	scanner := bufio.NewScanner(file)

	const maxLineSize = 1024 * 1024
	buf := make([]byte, maxLineSize)
	scanner.Buffer(buf, maxLineSize)

	for scanner.Scan() {
		logLines = append(logLines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		InternalNodeError(c, "Failed to read log file: "+err.Error())
		return
	}

	if filter != nil {
		matched, capped := filter.Apply(logLines)
		respondSuccess(c, "Logs retrieved", gin.H{
			"log_file":    logPath,
			"total_lines": len(logLines),
			"lines":       matched,
			"count":       len(matched),
			"truncated":   capped,
			"filtered":    true,
		})
		return
	}

	start := 0
	if len(logLines) > lines {
		start = len(logLines) - lines
	}
	resultLines := logLines[start:]

	respondSuccess(c, "Logs retrieved", gin.H{
		"log_file":    logPath,
		"total_lines": len(logLines),
		"lines":       resultLines,
		"count":       len(resultLines),
	})
}

// readLastNLinesEfficient reads the last N lines from a large file efficiently.
func readLastNLinesEfficient(file *os.File, n int) (lines []string, truncated bool, err error) {
	const bufSize = 8192
	buf := make([]byte, bufSize)

	stat, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	fileSize := stat.Size()

	var currentLine []byte
	capHit := false

	offset := fileSize
reader:
	for offset > 0 {
		chunkSize := min(offset, int64(bufSize))
		offset -= chunkSize

		if _, err = file.Seek(offset, 0); err != nil {
			return nil, false, err
		}
		bytesRead, err := file.Read(buf[:chunkSize])
		if err != nil && err != io.EOF {
			return nil, false, err
		}

		for i := bytesRead - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				if len(currentLine) > 0 || i < bytesRead-1 {
					reversedLine := make([]byte, len(currentLine))
					for j := 0; j < len(currentLine); j++ {
						reversedLine[j] = currentLine[len(currentLine)-1-j]
					}
					lines = append(lines, string(reversedLine))
					currentLine = currentLine[:0]
					if len(lines) >= n {
						capHit = true
						break reader
					}
				}
			} else {
				currentLine = append(currentLine, buf[i])
			}
		}
	}

	if len(currentLine) > 0 {
		reversedLine := make([]byte, len(currentLine))
		for j := 0; j < len(currentLine); j++ {
			reversedLine[j] = currentLine[len(currentLine)-1-j]
		}
		lines = append(lines, string(reversedLine))
	}

	for i := 0; i < len(lines)/2; i++ {
		lines[i], lines[len(lines)-1-i] = lines[len(lines)-1-i], lines[i]
	}

	truncated = capHit && offset > 0
	return lines, truncated, nil
}

// streamInstanceLogs streams log file content in real-time (SSE).
func (l *LogsHandlers) streamInstanceLogs(c *gin.Context, logPath string, filter *logFilter) {
	logManager := l.appMgr.LogManager()
	if logManager != nil {
		baseDir := logManager.GetBaseDir()
		realLogPath, err1 := filepath.EvalSymlinks(logPath)
		realBaseDir, err2 := filepath.EvalSymlinks(baseDir)
		if err1 != nil {
			realLogPath, err1 = filepath.Abs(logPath)
		}
		if err2 != nil {
			realBaseDir, err2 = filepath.Abs(baseDir)
		}
		if err1 == nil && err2 == nil {
			if !strings.HasPrefix(realLogPath, realBaseDir) {
				Forbidden(c, "Access denied: Log file path outside allowed directory")
				return
			}
		}
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	file, err := os.Open(logPath)
	if err != nil {
		c.SSEvent("error", "Failed to open log file")
		return
	}
	defer func() { _ = file.Close() }()

	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		c.SSEvent("error", "Failed to seek log file")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterDefaultTimeout)
	defer cancel()

	reader := bufio.NewReader(file)
	ticker := time.NewTicker(constants.StatusPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.SSEvent("done", "Stream closed")
			return
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						break
					}
					c.SSEvent("error", "Log read error")
					return
				}

				trimmed := strings.TrimRight(line, "\n")
				if !filter.Match(trimmed) {
					continue
				}
				c.SSEvent("log", trimmed)
				c.Writer.Flush()
			}
		}
	}
}

// HandleGetRunProbesPublic returns probe status for a run.
//
// Public-side handler for GET /runs/:id/probes: when ?node=W is set,
// dispatches to the worker's internal mTLS path
// /zzrouter/v1/internal/runs/<id>/probes via s.cluster.router.Unicast
// (handleClusterAction). Worker registers the internal handler in
// routes_internal.go; the admin port is localhost-only post-pairing so
// direct cross-host reach to /zzrouter/v1/runs is unreachable.
func (l *LogsHandlers) HandleGetRunProbesPublic(c *gin.Context) {
	if l.appMgr == nil {
		ServiceUnavailable(c, "provider manager not initialized")
		return
	}

	params := ValidateParams(c, []ParamRule{RequiredPathParam("id")})
	if params == nil {
		return
	}
	instanceID := params.GetString("id")

	host := QueryNode(c)
	if l.handleClusterAction(c, host, fmt.Sprintf("/zzrouter/v1/internal/runs/%s/probes", instanceID), "GET", nil) {
		return
	}

	l.respondLocalProbeStatus(c, instanceID)
}

// handleInternalGetRunProbes is the local-only counterpart served on
// the cluster mTLS internal engine. Coord's public handler dispatches
// here via Unicast.
func (l *LogsHandlers) handleInternalGetRunProbes(c *gin.Context) {
	if l.appMgr == nil {
		ServiceUnavailable(c, "provider manager not initialized")
		return
	}

	params := ValidateParams(c, []ParamRule{RequiredPathParam("id")})
	if params == nil {
		return
	}
	l.respondLocalProbeStatus(c, params.GetString("id"))
}

func (l *LogsHandlers) respondLocalProbeStatus(c *gin.Context, instanceID string) {
	inst, exists := l.appMgr.GetInstance(instanceID)
	if !exists {
		NotFound(c, fmt.Sprintf("Instance not found: Instance %s not found", instanceID))
		return
	}
	respondSuccess(c, "Probe status retrieved", gin.H{
		"instance_id": instanceID,
		"status":      string(inst.GetStatus()),
		"started_at":  inst.StartedAt.Format(time.RFC3339),
		"log_file":    inst.LogFilePath,
	})
}

// findInstanceOnCluster queries cluster nodes to find instance data.
func (l *LogsHandlers) findInstanceOnCluster(instanceID string) map[string]any {
	clusterClient := l.getClusterClient()
	if clusterClient == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()

	instances, err := mesh.AggregateArrayField(ctx, clusterClient, "/zzrouter/v1/internal/runs", "instances", mesh.QueryParams{
		Method: "GET",
	})
	if err != nil {
		return nil
	}

	for _, inst := range instances {
		if id, ok := inst["id"].(string); ok && id == instanceID {
			return inst
		}
	}

	return nil
}

// routeInstanceLogsToCluster forwards static logs request to the cluster host.
func (l *LogsHandlers) routeInstanceLogsToCluster(c *gin.Context, targetNode, instanceID string, lines int, follow bool) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.HTTPDefaultTimeout)
	defer cancel()

	query := mergeClusterLogQuery(c.Request.URL.Query(), lines, follow)
	endpoint := fmt.Sprintf("/zzrouter/v1/internal/runs/%s/logs?%s", instanceID, query.Encode())

	resp, err := l.routeToClusterNode(ctx, targetNode, endpoint, "GET", nil, c.Request.Header)
	if err != nil {
		BadGateway(c, fmt.Sprintf("Failed to get logs from host '%s': %s", targetNode, err.Error()))
		return
	}

	c.Data(resp.StatusCode, "application/json", resp.Body)
}

// proxySSELogsFromCluster proxies SSE log stream from remote cluster node.
func (l *LogsHandlers) proxySSELogsFromCluster(c *gin.Context, targetNode, instanceID string, lines int) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	clusterNode, err := l.getClusterNodeURL(targetNode)
	if err != nil {
		c.SSEvent("error", "Failed to resolve cluster host")
		return
	}

	query := mergeClusterLogQuery(c.Request.URL.Query(), lines, true)
	logURL := fmt.Sprintf("%s/zzrouter/v1/internal/runs/%s/logs?%s", clusterNode, instanceID, query.Encode())

	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", logURL, nil)
	if err != nil {
		c.SSEvent("error", "Failed to create request")
		return
	}

	resp, err := l.httpStreamingClient.Do(req)
	if err != nil {
		c.SSEvent("error", "Failed to connect to worker")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.SSEvent("error", fmt.Sprintf("Worker returned status %d", resp.StatusCode))
		return
	}

	reader := bufio.NewReader(resp.Body)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.SSEvent("error", "Streaming not supported")
		return
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				c.SSEvent("done", "Stream ended")
			} else {
				c.SSEvent("error", "Stream error")
			}
			return
		}

		_, _ = c.Writer.Write([]byte(line))
		flusher.Flush()

		select {
		case <-c.Request.Context().Done():
			return
		default:
		}
	}
}

// mergeClusterLogQuery takes the incoming request's query params and
// returns a fresh url.Values with `lines` and `follow` set exactly once.
func mergeClusterLogQuery(incoming url.Values, lines int, follow bool) url.Values {
	out := make(url.Values, len(incoming))
	for k, v := range incoming {
		if k == "lines" || k == "follow" {
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	out.Set("lines", fmt.Sprintf("%d", lines))
	out.Set("follow", fmt.Sprintf("%t", follow))
	return out
}
