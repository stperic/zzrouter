package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ============================================================================
// Quickstart tutorial page content — styled at render time
// ============================================================================

// tutorialPage defines one page of the quickstart tutorial.
type tutorialPage struct {
	// renderBody returns the styled body content for this page.
	// width and height are available space (excluding title bar and footer).
	renderBody func(width, height int, styles ui.Styles, shimmerFrame int) string
	centered   bool   // center content vertically
	hint       string // footer hint text (unset = default)
}

const totalTutorialPages = 4

func tutorialPageList() []tutorialPage {
	return []tutorialPage{
		pageWelcome(),
		pageSingleNode(),
		pageCluster(),
		pageTransition(),
	}
}

// --- Page 0: Welcome ---

func pageWelcome() tutorialPage {
	return tutorialPage{
		centered: true,
		renderBody: func(width, _ int, styles ui.Styles, shimmerFrame int) string {
			var b strings.Builder

			title := ui.Shimmer("zzRouter", shimmerFrame, "#d4a574")
			titleStyled := lipgloss.NewStyle().Bold(true).Render(title)
			b.WriteString(titleStyled + "\n")
			b.WriteString(shared.Hrule(styles, 28) + "\n\n")

			b.WriteString(styles.Normal.Render("Your AI model router.") + "\n")
			b.WriteString(styles.Normal.Render("One endpoint for every LLM, local or cloud.") + "\n\n")

			b.WriteString(styles.Help.Render("Route requests to Ollama, vLLM, llama.cpp, MLX,") + "\n")
			b.WriteString(styles.Help.Render("OpenAI, Anthropic, Azure, and more.") + "\n")
			b.WriteString(styles.Help.Render("Manage a cluster of GPU nodes from a single CLI.") + "\n")

			return b.String()
		},
	}
}

// ============================================================================
// Diagram builder helpers — uses lipgloss borders for clean rendering
// ============================================================================

// padToWidth pads a styled string with spaces to reach targetWidth.
// Uses lipgloss.Width for correct ANSI-aware measurement.
func padToWidth(s string, targetWidth int) string {
	w := lipgloss.Width(s)
	if w >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-w)
}

// themedBoxStyle creates a lipgloss style for a diagram box with rounded borders.
func themedBoxStyle(borderColor lipgloss.Style, minWidth int) lipgloss.Style {
	s := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor.GetForeground()).
		Padding(0, 1)
	if minWidth > 0 {
		s = s.Width(minWidth)
	}
	return s
}

// padLines pads content to targetLines by appending empty lines.
func padLines(content string, targetLines int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < targetLines {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// maxContentLines returns the max line count across multiple content strings.
func maxContentLines(contents ...string) int {
	m := 0
	for _, c := range contents {
		n := strings.Count(c, "\n") + 1
		if n > m {
			m = n
		}
	}
	return m
}

// centeredArrow returns a ──▶ arrow vertically centered to match boxHeight.
func centeredArrow(boxHeight int, arrowStyle lipgloss.Style) string {
	arrow := arrowStyle.Render("──▶")
	return lipgloss.Place(lipgloss.Width(arrow)+2, boxHeight, lipgloss.Center, lipgloss.Center, arrow)
}

// --- Page 1: Single Node Architecture ---

func pageSingleNode() tutorialPage {
	return tutorialPage{
		renderBody: func(width, _ int, styles ui.Styles, _ int) string {
			var b strings.Builder

			b.WriteString(styles.Title.Render("How it works") + "\n\n")
			b.WriteString(" " + styles.Normal.Render("Agents connect to zzRouter using standard APIs.") + "\n")
			b.WriteString(" " + styles.Normal.Render("zzRouter routes each request to the right provider.") + "\n\n")

			var c1, c2, c3 string
			var w1, w2, w3 int

			if width >= 80 {
				c1 = "Claude Code\nCursor\nOpen WebUI\nOpenAI clients"
				c2 = "Router\nLoad Balancer\nHealth Monitor"
				c3 = "Ollama  " + styles.Help.Render("llama3.2") +
					"\nvLLM    " + styles.Help.Render("mistral") +
					"\nMLX     " + styles.Help.Render("phi-4")
				w1, w2, w3 = 18, 18, 20
			} else {
				c1 = "Claude\nCursor\nWebUI"
				c2 = "Router\nBalance\nHealth"
				c3 = "Ollama\nvLLM\nMLX"
				w1, w2, w3 = 0, 0, 0
			}

			// Equal-height boxes
			ml := maxContentLines(c1, c2, c3)
			box1 := themedBoxStyle(styles.DetailValue, w1).Render(padLines(c1, ml))
			box2 := themedBoxStyle(styles.Title, w2).Render(padLines(c2, ml))
			box3 := themedBoxStyle(styles.Success, w3).Render(padLines(c3, ml))

			// Centered arrows
			boxH := lipgloss.Height(box1)
			arrow := centeredArrow(boxH, styles.Help)

			// Labels
			wB1A := lipgloss.Width(box1) + lipgloss.Width(arrow)
			wB2A := lipgloss.Width(box2) + lipgloss.Width(arrow)
			labels := padToWidth(styles.DetailValue.Bold(true).Render("Agents"), wB1A) +
				padToWidth(styles.Title.Bold(true).Render("zzRouter"), wB2A) +
				styles.Success.Bold(true).Render("Providers")
			b.WriteString(" " + labels + "\n")

			diagram := lipgloss.JoinHorizontal(lipgloss.Top, box1, arrow, box2, arrow, box3)
			b.WriteString(" " + diagram + "\n\n")

			if width >= 80 {
				b.WriteString(" " + styles.Help.Render("OpenAI-compatible API (POST /v1/chat/completions)") + "\n")
			} else {
				b.WriteString(" " + styles.Help.Render("OpenAI-compatible API (/v1/*)") + "\n")
			}

			return b.String()
		},
	}
}

// --- Page 2: Cluster + Cloud ---

func pageCluster() tutorialPage {
	return tutorialPage{
		renderBody: func(width, _ int, styles ui.Styles, _ int) string {
			var b strings.Builder

			b.WriteString(styles.Title.Render("Scale across nodes and clouds") + "\n\n")
			b.WriteString(" " + styles.Normal.Render("Add GPU nodes to your cluster. Fall back to cloud when needed.") + "\n\n")

			var agentsBox, coordBox string
			var w1Box, w2Box, cloudBox string

			if width >= 80 {
				agentsBox = themedBoxStyle(styles.DetailValue, 16).Render(
					"Claude Code\nCursor\nOpen WebUI")
				coordBox = themedBoxStyle(styles.Title, 18).Render(
					"zzRouter\n" + styles.Help.Render("coordinator") +
						"\n\nRoutes to best\nnode available\n\nAuto-failover\nHealth checks")
				w1Box = themedBoxStyle(styles.Success, 18).Render(
					styles.Success.Render("node-1") + "\nRTX 4090\nvLLM + Ollama")
				w2Box = themedBoxStyle(styles.Success, 18).Render(
					styles.Success.Render("node-2") + "\nApple M4 Ultra\nMLX")
				cloudBox = themedBoxStyle(styles.Accent, 18).Render(
					styles.Accent.Render("Cloud") + "\nOpenAI / Azure\nAnthropic")
			} else {
				agentsBox = themedBoxStyle(styles.DetailValue, 0).Render(
					"Claude\nCursor\nWebUI")
				coordBox = themedBoxStyle(styles.Title, 0).Render(
					"zzRouter\n" + styles.Help.Render("coord.") +
						"\n\nRoutes +\nfailover")
				w1Box = themedBoxStyle(styles.Success, 14).Render(
					styles.Success.Render("node-1") + "\nRTX 4090")
				w2Box = themedBoxStyle(styles.Success, 14).Render(
					styles.Success.Render("node-2") + "\nM4 Ultra")
				cloudBox = themedBoxStyle(styles.Accent, 14).Render(
					styles.Accent.Render("Cloud") + "\nOpenAI\nAnthropic")
			}

			// Right column: stack workers + cloud
			rightCol := w1Box + "\n" + w2Box + "\n" + cloudBox

			// Centered arrows
			coordH := lipgloss.Height(coordBox)
			arrow1 := centeredArrow(lipgloss.Height(agentsBox), styles.Help)
			arrow2 := centeredArrow(coordH, styles.Help)

			// Labels
			wAgentsArrow := lipgloss.Width(agentsBox) + lipgloss.Width(arrow1)
			wCoordArrow := lipgloss.Width(coordBox) + lipgloss.Width(arrow2)
			labels := padToWidth(styles.DetailValue.Bold(true).Render("Agents"), wAgentsArrow) +
				padToWidth(styles.Title.Bold(true).Render("Coordinator"), wCoordArrow) +
				styles.Success.Bold(true).Render("Workers + Cloud")
			b.WriteString(" " + labels + "\n")

			diagram := lipgloss.JoinHorizontal(lipgloss.Top, agentsBox, arrow1, coordBox, arrow2, rightCol)
			b.WriteString(" " + diagram + "\n")

			return b.String()
		},
	}
}

// --- Page 3: Transition to wizard ---

func pageTransition() tutorialPage {
	return tutorialPage{
		centered: true,
		renderBody: func(width, _ int, styles ui.Styles, shimmerFrame int) string {
			var b strings.Builder

			title := ui.Shimmer("Let's set up your first node", shimmerFrame, "#d4a574")
			titleStyled := lipgloss.NewStyle().Bold(true).Render(title)
			b.WriteString(titleStyled + "\n")
			b.WriteString(shared.Hrule(styles, 32) + "\n\n")

			b.WriteString(styles.Normal.Render("We'll walk you through:") + "\n\n")

			steps := []string{
				"Detecting your hardware (GPU, CPU)",
				"Choosing a provider (Ollama, vLLM, MLX...)",
				"Pulling your first model",
				"Sending your first request",
			}
			for i, s := range steps {
				num := styles.Success.Render(fmt.Sprintf("%d", i+1))
				fmt.Fprintf(&b, "  %s  %s\n", num, styles.Normal.Render(s))
			}

			b.WriteString("\n")
			b.WriteString(styles.Help.Render("This takes about 2 minutes.") + "\n")

			return b.String()
		},
		hint: "Enter to begin setup",
	}
}

// --- Completion screen content ---

func renderCompletionScreen(width, height int, styles ui.Styles) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	b.WriteString(styles.Success.Bold(true).Render("  ✔ You're all set!") + "\n\n")

	// Left column: TUI menu
	var left strings.Builder
	left.WriteString(styles.Title.Render("TUI Dashboard") + "\n")
	left.WriteString(styles.Help.Render("zzrouter tui") + "\n\n")

	menuItems := []struct{ name, key string }{
		{"Models", "1"},
		{"Search", "2"},
		{"Providers", "3"},
		{"Nodes", "4"},
		{"Logs", "5"},
		{"Chat", "6"},
	}
	for _, item := range menuItems {
		fmt.Fprintf(&left, "  %s  %s\n",
			styles.Help.Render(item.key),
			styles.Normal.Render(item.name))
	}
	left.WriteString("\n")
	left.WriteString(styles.Help.Render("Navigate with arrows or") + "\n")
	left.WriteString(styles.Help.Render("number keys + Enter") + "\n")

	// Right column: Skills
	var right strings.Builder
	right.WriteString(styles.DetailValue.Render("Skills") + "\n")
	right.WriteString(styles.Help.Render("Built-in integrations") + "\n\n")

	skills := []struct{ cmd, desc string }{
		{"/claude-code", "Connect Claude Code"},
		{"/api", "OpenAI-compatible API"},
		{"/search", "Find & deploy models"},
		{"/chat", "Chat in your terminal"},
	}
	for _, s := range skills {
		fmt.Fprintf(&right, "  %s\n", styles.Title.Render(s.cmd))
		fmt.Fprintf(&right, "  %s\n\n", styles.Help.Render(s.desc))
	}

	leftStr := left.String()
	rightStr := right.String()

	colWidth := max(width/2-4, 30)
	leftBox := lipgloss.NewStyle().Width(colWidth).Render(leftStr)
	rightBox := lipgloss.NewStyle().Width(colWidth).Render(rightStr)
	columns := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox)

	b.WriteString(columns)

	return b.String()
}
