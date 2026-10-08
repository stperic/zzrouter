package clientcli

import (
	"context"
	"fmt"
	"strings"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// validateNotSubcommand checks if the model name looks like a subcommand or reserved word.
func validateNotSubcommand(modelName string) error {
	reserved := map[string]string{
		"status":  "Use 'zzrouter deploy status' to show download progress",
		"ps":      "Use 'zzrouter ps' to show running models, or 'zzrouter deploy status' for downloads",
		"list":    "Use 'zzrouter ls' to list available models",
		"ls":      "Use 'zzrouter ls' to list available models",
		"clean":   "Use 'zzrouter deploy clean' to clean completed downloads",
		"stop":    "Use 'zzrouter deploy stop' to stop ongoing downloads",
		"help":    "Use 'zzrouter deploy --help' for help",
		"version": "Use 'zzrouter version' to show version",
	}

	modelLower := strings.ToLower(strings.TrimSpace(modelName))

	if suggestion, isReserved := reserved[modelLower]; isReserved {
		return fmt.Errorf("'%s' is not a valid model name.\n\n%s %s", modelName, ui.GetInfoEmoji(), suggestion)
	}

	return nil
}

// NewDeployCmd creates the deploy command group for downloading models.
func NewDeployCmd() *cobra.Command {
	var repo string
	var model string
	var node string
	var file string
	var toNodes []string
	var toAll bool
	var force bool
	var watch bool

	cmd := &cobra.Command{
		Use:     "deploy [model]",
		Aliases: []string{"pull"},
		Short:   "Deploy and download model(s)",
		Long: `Download language models from Ollama or HuggingFace repositories.

FLEXIBLE MODEL NOTATION:
  1. Simple model name:          llama2:latest
  2. Repo/model:                 hf/meta-llama/Llama-3:8b
  3. Model@node:                 llama2@gpu-server
  4. Node::repo/model:           localhost::ollama/gemma:7b
  5. Full notation:              gpu-server::hf/microsoft/phi-2

REPOSITORIES:
  ollama       # Ollama model registry (default for simple names)
  hf           # HuggingFace model hub (alias: huggingface)
  gguf         # GGUF format models
  local        # Local models

AUTO-DETECTION:
  Models with '/' are auto-detected as HuggingFace (e.g., org/model-name)
  Models without '/' default to Ollama (e.g., llama2:latest)

VARIANT SELECTION:
  --file <filename>             # Download specific variant file

OVERRIDE FLAGS:
  --node <name>                 # Single-node deploy
  --provider <name>             # Override auto-detected provider
  --file <filename>             # Specific GGUF variant file

MULTI-NODE DEPLOY:
  --to <node1,node2,...>        # Deploy to specific nodes
  --to-all                      # Deploy to all compatible nodes

USAGE:
  zzrouter deploy llama2:latest
  zzrouter deploy hf/meta-llama/Llama-3:8b
  zzrouter deploy --node gpu-server llama2:latest
  zzrouter deploy bartowski/Llama-3.2-3B-Instruct-GGUF --file llama-3.2-3b-instruct.Q4_K_M.gguf
  zzrouter deploy llama2 --to-all
  zzrouter deploy llama2 --to gpu-1,gpu-2,gpu-3`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modelIdentifier := model
			if len(args) > 0 {
				modelIdentifier = args[0]
			}

			if modelIdentifier == "" {
				return cmd.Help()
			}

			if err := validateNotSubcommand(modelIdentifier); err != nil {
				return err
			}

			// Pass model string through unchanged. The server parser
			// handles #file, @node, and registry detection.
			return runDeploy(modelIdentifier, repo, file, node, toNodes, toAll, force, watch)
		},
	}

	cmd.Flags().StringVarP(&repo, "provider", "a", "", "Provider (auto-detected: ollama for simple names, huggingface for org/model)")
	cmd.Flags().StringVar(&model, "model", "", "Model name to download")
	cmd.Flags().StringVarP(&node, "node", "n", "", "Target node in cluster")
	cmd.Flags().StringVar(&file, "file", "", "Specific file/variant to download")
	cmd.Flags().StringSliceVar(&toNodes, "to", nil, "Target nodes for multi-node deploy (comma-separated)")
	cmd.Flags().BoolVar(&toAll, "to-all", false, "Deploy to all compatible nodes in cluster")
	cmd.Flags().BoolVar(&force, "force", false, "Download even if no compatible provider exists on the target node")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Stream live progress until the deploy completes (single-node only)")

	cmd.AddCommand(
		newDeployStatusCmd(),
		newDeployStopCmd(),
		newDeployVariantsCmd(),
	)

	return cmd
}

func newDeployStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [job_id]",
		Short: "Show status of ongoing downloads and deploy jobs",
		Long: `Display the progress and status of deploy jobs.

Without arguments, lists every job. With a job ID, shows per-node detail.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return runDeploymentDetail(args[0])
			}
			return runDeployStatus(nil)
		},
	}
}

func newDeployVariantsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "variants <org/repo>",
		Short: "List GGUF quantization variants available in a HuggingFace repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployVariants(args[0])
		},
	}
}

func runDeployVariants(modelID string) error {
	renderer := ui.NewRenderer(ui.RendererOptions{Adaptive: true, MinWidth: 80})
	defer func() { _ = renderer.Close() }()

	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	resp, err := client.GetHuggingFaceVariants(modelID)
	if err != nil {
		return fmt.Errorf("%s", shared.ExtractUserFriendlyError(err))
	}

	if len(resp.Variants) == 0 {
		_ = renderer.ShowMessage(fmt.Sprintf("No GGUF variants found in %s (non-GGUF files: %d)", resp.ModelID, resp.NonGGUFFiles), ui.LevelWarning)
		return nil
	}

	fmt.Printf("\nRepo: %s\n", resp.ModelID)
	fmt.Printf("GGUF variants: %d | Non-GGUF files: %d\n\n", len(resp.Variants), resp.NonGGUFFiles)

	tableData := ui.TableData{
		Columns: []ui.TableColumn{
			{Header: "FILE", MaxWidth: 50},
			{Header: "QUANT", MaxWidth: 10},
			{Header: "BITS", MaxWidth: 6},
			{Header: "SIZE", MaxWidth: 10},
			{Header: "PICK", MaxWidth: 0},
		},
		MinColumns: 4,
		Rows:       [][]string{},
	}

	var recommendedFile, recommendedReason string
	for _, v := range resp.Variants {
		pick := ""
		if v.Recommended {
			pick = "← " + v.RecommendationReason
			recommendedFile = v.File
			recommendedReason = v.RecommendationReason
		}
		bitsStr := "-"
		if v.Bits > 0 {
			bitsStr = fmt.Sprintf("%d", v.Bits)
		}
		quant := v.Quantization
		if quant == "" {
			quant = "-"
		}
		tableData.Rows = append(tableData.Rows, []string{
			ui.TruncateString(v.File, 50),
			quant,
			bitsStr,
			v.SizeHuman,
			pick,
		})
	}

	if err := renderer.RenderTable(tableData); err != nil {
		return fmt.Errorf("failed to render table: %w", err)
	}

	fmt.Println()
	if recommendedFile != "" {
		_ = renderer.ShowMessage(fmt.Sprintf("Recommended: %s (%s)", recommendedFile, recommendedReason), ui.LevelInfo)
		fmt.Printf("\nDeploy it with:\n  zzrouter deploy %s --file %s\n\n", resp.ModelID, recommendedFile)
	} else {
		fmt.Printf("Deploy one with:\n  zzrouter deploy %s --file <FILE>\n\n", resp.ModelID)
	}
	return nil
}

func newDeployStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop all active deploy jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployStop(nil)
		},
	}
}

// runDeploy is the single unified path for all deploy invocations. It
// resolves Nodes client-side (via --node/--to/--to-all or interactive
// selectCompatibleNode) so the server never needs to infer targets.
func runDeploy(modelIdentifier, repository, file, singleNode string, toNodes []string, toAll, force, watch bool) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	var nodes []string
	switch {
	case toAll:
		compatibleNodes, err := client.GetCompatibleNodes(modelIdentifier, repository, "")
		if err != nil {
			return fmt.Errorf("failed to fetch compatible nodes: %w", err)
		}
		if len(compatibleNodes) == 0 {
			return fmt.Errorf("no compatible nodes found for model '%s'", modelIdentifier)
		}
		for _, host := range compatibleNodes {
			nodes = append(nodes, host.Name)
		}
		fmt.Printf("Found %d compatible nodes: %s\n\n", len(nodes), strings.Join(nodes, ", "))
		// --to-all is destructive: every compatible node triggers a
		// download. Confirm unless --force was passed; deny by default
		// on non-TTY so CI runs don't fire off mass deploys silently.
		if !force {
			prompt := fmt.Sprintf("Deploy '%s' to all %d nodes? [y/N]: ", modelIdentifier, len(nodes))
			if !confirmYN(prompt) {
				fmt.Println("Aborted.")
				return nil
			}
		}
	case len(toNodes) > 0:
		nodes = toNodes
	case singleNode != "":
		nodes = []string{singleNode}
	default:
		selected, err := selectCompatibleNode(client, modelIdentifier, repository)
		if err != nil {
			return err
		}
		nodes = []string{selected}
	}

	resp, err := client.Deploy(modelIdentifier, repository, file, nodes, force)
	if err != nil {
		return fmt.Errorf("%s", shared.ExtractUserFriendlyError(err))
	}

	fmt.Printf("%s Deploy job started\n\n", ui.GetCheckEmoji())
	fmt.Printf("  Job ID: %s\n", resp.ID)
	fmt.Printf("  Model: %s\n", resp.Model)
	if len(resp.Nodes) > 0 {
		nodeNames := make([]string, len(resp.Nodes))
		for i, n := range resp.Nodes {
			nodeNames[i] = n.Node
		}
		fmt.Printf("  Nodes: %s\n", strings.Join(nodeNames, ", "))
	}
	fmt.Printf("  Status: %s\n\n", resp.Status)

	renderer := ui.NewRenderer(ui.RendererOptions{Adaptive: true, MinWidth: 80})
	defer func() { _ = renderer.Close() }()

	if watch {
		if len(resp.Nodes) == 0 {
			_ = renderer.ShowMessage("--watch: no node progress entries returned; skipping live stream", ui.LevelWarning)
			fmt.Println()
			return nil
		}
		if len(resp.Nodes) > 1 {
			_ = renderer.ShowMessage("--watch: streaming first node only (multi-node watch not implemented)", ui.LevelInfo)
		}
		target := resp.Nodes[0]
		if target.JobID == "" {
			_ = renderer.ShowMessage("--watch: server did not return a job_id (provider may be sync); falling back to 'deploy status'", ui.LevelInfo)
			_ = renderer.ShowMessage(fmt.Sprintf("Use 'zzrouter deploy status %s' to monitor progress", resp.ID), ui.LevelInfo)
			fmt.Println()
			return nil
		}
		label := fmt.Sprintf("Deploying %s to %s", resp.Model, target.Node)
		return RenderJobProgress(context.Background(), client, target.JobID, target.Node, label)
	}

	_ = renderer.ShowMessage(fmt.Sprintf("Use 'zzrouter deploy status %s' to monitor progress", resp.ID), ui.LevelInfo)
	fmt.Println()
	return nil
}

// selectCompatibleNode shows compatible hosts and prompts user to select one.
func selectCompatibleNode(client *pkgClient.Client, modelName, repository string) (string, error) {
	renderer := ui.NewRenderer(ui.RendererOptions{
		Adaptive: true,
		MinWidth: 80,
	})
	defer func() { _ = renderer.Close() }()

	compatibleNodes, err := client.GetCompatibleNodes(modelName, repository, "")
	if err != nil {
		return "", fmt.Errorf("failed to fetch compatible nodes: %w", err)
	}

	if len(compatibleNodes) == 0 {
		return "", fmt.Errorf("no compatible nodes found for model '%s'", modelName)
	}

	if len(compatibleNodes) == 1 {
		selectedNode := compatibleNodes[0].Name
		fmt.Printf("Auto-selected node: %s (only compatible node)\n\n", selectedNode)
		return selectedNode, nil
	}

	fmt.Printf("\nModel: %s\n", modelName)
	repositoryDisplay := repository
	if repositoryDisplay == "" {
		repositoryDisplay = "(auto-detect)"
	}
	fmt.Printf("Format: %s | Repository: %s\n\n", detectFormat(modelName), repositoryDisplay)
	fmt.Printf("Select download destination:\n\n")

	tableData := ui.TableData{
		Columns: []ui.TableColumn{
			{Header: "#", MaxWidth: 4},
			{Header: "NODE", MaxWidth: 20},
			{Header: "PROVIDERS", MaxWidth: 30},
			{Header: "DISK FREE", MaxWidth: 0},
		},
		MinColumns: 4,
		Rows:       [][]string{},
	}

	for i, host := range compatibleNodes {
		providerTypes := make([]string, len(host.Providers))
		for j, p := range host.Providers {
			providerTypes[j] = p.Type
		}
		providersDisplay := strings.Join(providerTypes, ", ")
		if providersDisplay == "" {
			providersDisplay = "(no providers)"
		}

		diskDisplay := "-"
		if host.DiskInfo != nil {
			diskDisplay = fmt.Sprintf("%.1f GB", host.DiskInfo.AvailableGB)
		}

		tableData.Rows = append(tableData.Rows, []string{
			fmt.Sprintf("%d", i+1),
			host.Name,
			providersDisplay,
			diskDisplay,
		})
	}

	if err := renderer.RenderTable(tableData); err != nil {
		return "", fmt.Errorf("failed to render table: %w", err)
	}

	input := readLine(fmt.Sprintf("\nSelect node (1-%d) or press Enter to cancel: ", len(compatibleNodes)))
	if input == "" {
		return "", fmt.Errorf("download cancelled")
	}

	var selection int
	_, err = fmt.Sscanf(input, "%d", &selection)
	if err != nil || selection < 1 || selection > len(compatibleNodes) {
		return "", fmt.Errorf("invalid selection: %s (must be 1-%d)", input, len(compatibleNodes))
	}

	selectedNode := compatibleNodes[selection-1].Name
	fmt.Printf("Selected: %s\n\n", selectedNode)
	return selectedNode, nil
}

// detectFormat detects the format from model name (display helper).
func detectFormat(modelName string) string {
	if f := modelregistry.DetectFormatFromName(modelName); f != "" {
		return f
	}
	return "auto-detect"
}

func runDeployStatus(_ *pkgConfig.ClientConfig) error {
	renderer := ui.NewRenderer(ui.RendererOptions{
		Adaptive: true,
		MinWidth: 80,
	})
	defer func() { _ = renderer.Close() }()

	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	jobs, err := client.ListDeployments()
	if err != nil {
		_ = renderer.ShowMessage(fmt.Sprintf("Node is not reachable: %v", err), ui.LevelWarning)
		return nil
	}

	if len(jobs) == 0 {
		_ = renderer.ShowMessage("No deploy jobs found.", ui.LevelInfo)
		fmt.Println()
		return nil
	}

	tableData := ui.TableData{
		Columns: []ui.TableColumn{
			{Header: "JOB ID", MaxWidth: 15},
			{Header: "MODEL", MaxWidth: 35},
			{Header: "STATUS", MaxWidth: 15},
			{Header: "NODES", MaxWidth: 25},
			{Header: "PROGRESS", MaxWidth: 0},
		},
		MinColumns: 4,
		Rows:       [][]string{},
	}

	for _, job := range jobs {
		statusDisplay := fmt.Sprintf("%s %s", deploymentStatusEmoji(job.Status), job.Status)

		nodesStr := fmt.Sprintf("%d/%d complete", job.NodesComplete, job.NodesTotal)
		if job.NodesFailed > 0 {
			nodesStr += fmt.Sprintf(" (%d failed)", job.NodesFailed)
		}

		progressStr := "-"
		var bytesDownloaded, bytesTotal int64
		for _, n := range job.Nodes {
			bytesDownloaded += n.BytesDownloaded
			bytesTotal += n.BytesTotal
		}
		if bytesTotal > 0 {
			progressStr = fmt.Sprintf("%.1f%% (%s / %s)",
				float64(bytesDownloaded)/float64(bytesTotal)*100,
				shared.FormatSize(bytesDownloaded),
				shared.FormatSize(bytesTotal))
		}

		tableData.Rows = append(tableData.Rows, []string{
			job.ID,
			ui.TruncateString(utils.FormatModelName(job.Model), 35),
			statusDisplay,
			nodesStr,
			progressStr,
		})
	}

	if err := renderer.RenderTable(tableData); err != nil {
		return fmt.Errorf("failed to render table: %w", err)
	}

	fmt.Println()
	_ = renderer.ShowMessage("Use 'zzrouter deploy status <job_id>' to see per-node details", ui.LevelInfo)
	fmt.Println()

	return nil
}

func runDeployStop(_ *pkgConfig.ClientConfig) error {
	fmt.Println("Cancelling active deploy jobs...")

	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	if err := client.StopAllDeployments(""); err != nil {
		fmt.Printf("Node is not reachable: %v\n", err)
		return nil
	}

	fmt.Println("Cancelled active deploy jobs")
	return nil
}

// shared.DownloadOption represents a host/provider combination for downloading.

// runDeploymentDetail shows detailed status for a specific job.
func runDeploymentDetail(jobID string) error {
	renderer := ui.NewRenderer(ui.RendererOptions{
		Adaptive: true,
		MinWidth: 80,
	})
	defer func() { _ = renderer.Close() }()

	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	job, err := client.GetDeployment(jobID)
	if err != nil {
		return fmt.Errorf("failed to get job details: %w", err)
	}

	if job == nil {
		return fmt.Errorf("job not found: %s", jobID)
	}

	fmt.Printf("\nDeploy Job: %s\n", job.ID)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("  Model: %s\n", job.Model)
	fmt.Printf("  Format: %s\n", job.Format)
	fmt.Printf("  Status: %s - %s\n", job.Status, job.Message)
	fmt.Printf("  Created: %s\n", job.CreatedAt)
	fmt.Println()

	if len(job.Nodes) == 0 {
		_ = renderer.ShowMessage("No node progress data available.", ui.LevelWarning)
		return nil
	}

	fmt.Printf("Node Progress:\n")
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	tableData := ui.TableData{
		Columns: []ui.TableColumn{
			{Header: "#", MaxWidth: 4},
			{Header: "NODE", MaxWidth: 20},
			{Header: "STATUS", MaxWidth: 15},
			{Header: "SOURCE", MaxWidth: 15},
			{Header: "PROGRESS", MaxWidth: 10},
			{Header: "SPEED", MaxWidth: 0},
			{Header: "ETA", MaxWidth: 0},
		},
		MinColumns: 4,
		Rows:       [][]string{},
	}

	for i, node := range job.Nodes {
		statusDisplay := fmt.Sprintf("%s %s", deploymentNodeStatusEmoji(node.Status), node.Status)

		sourceDisplay := node.Source
		if sourceDisplay == "internet" {
			sourceDisplay = "🌐 internet"
		} else if sourceDisplay != "" {
			sourceDisplay = "🔗 " + sourceDisplay
		}

		progressStr := fmt.Sprintf("%.1f%%", node.Progress)
		if node.Progress == 0 && node.Status == constants.StatusPending {
			progressStr = "-"
		}

		speedStr := "-"
		if node.Speed > 0 {
			speedStr = shared.FormatSpeed(node.Speed)
		}

		etaStr := "-"
		if node.Status == constants.StatusDownloading && node.BytesTotal > 0 && node.Speed > 0 {
			remaining := node.BytesTotal - node.BytesDownloaded
			if remaining > 0 {
				etaStr = shared.FormatDuration(time.Duration(remaining/node.Speed)*time.Second, true)
			}
		}

		tableData.Rows = append(tableData.Rows, []string{
			fmt.Sprintf("%d", i+1),
			node.Node,
			statusDisplay,
			sourceDisplay,
			progressStr,
			speedStr,
			etaStr,
		})
	}

	if err := renderer.RenderTable(tableData); err != nil {
		return fmt.Errorf("failed to render table: %w", err)
	}

	fmt.Printf("\nSummary: %d complete, %d in progress, %d failed (%d total)\n",
		job.NodesComplete, job.NodesInProgress, job.NodesFailed, job.NodesTotal)
	fmt.Println()

	return nil
}

// "syncing" and "verifying" are transitional labels emitted by the download
// manager that have no first-class constant in pkg/deployment — they're
// intermediate states inside the downloading phase.
const (
	statusSyncing   = "syncing"
	statusVerifying = "verifying"
)

func deploymentStatusEmoji(status constants.Status) string {
	switch status {
	case constants.StatusPending:
		return "⏳"
	case constants.StatusDownloading:
		return "⬇️"
	case constants.Status(statusSyncing):
		return "🔄"
	case constants.StatusCompleted:
		return ui.GetCheckEmoji()
	case constants.StatusFailed:
		return "❌"
	case constants.StatusCancelled:
		return "🚫"
	}
	return ""
}

func deploymentNodeStatusEmoji(status constants.Status) string {
	switch status {
	case constants.StatusPending:
		return "⏳"
	case constants.StatusDownloading:
		return "⬇️"
	case constants.Status(statusSyncing):
		return "🔄"
	case constants.Status(statusVerifying):
		return "🔍"
	case constants.StatusCompleted:
		return ui.GetCheckEmoji()
	case constants.StatusFailed:
		return "❌"
	case constants.StatusSkipped:
		return "⏭️"
	}
	return ""
}
