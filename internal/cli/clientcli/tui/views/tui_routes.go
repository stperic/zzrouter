package views

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// ---------------------------------------------------------------------------
// Message types
// ---------------------------------------------------------------------------

// routesSavedMsg carries the result of saving a model group.
type routesSavedMsg struct {
	ctx      context.Context
	group    *pkgClient.ModelGroupResponse
	err      error
	fromMode modelsViewMode // which mode triggered the save
}

// routesDeletedMsg carries the result of deleting a model group.
type routesDeletedMsg struct {
	ctx context.Context
	err error
}

// routesPickerDataMsg carries pre-fetched model list for picker fields.
type routesPickerDataMsg struct {
	models []pkgClient.ModelMetadata
}

// ---------------------------------------------------------------------------
// Form field enums
// ---------------------------------------------------------------------------

// routeFormField identifies which text field is focused in the route overlay form.
type routeFormField int

const (
	routeFieldName routeFormField = iota
	routeFieldDescription
	routeFieldStrategy
)

// deployFormField identifies which text field is focused in the add-deployment form.
type deployFormField int

const (
	deployFieldName  deployFormField = iota
	deployFieldModel                 // ←/→ cycle picker through pulled models
	deployFieldPriority
	deployFieldTimeout
	deployFieldMaxRetries
	deployFieldTags
)

// ---------------------------------------------------------------------------
// Strategies
// ---------------------------------------------------------------------------

var routeStrategies = []string{pkgClient.StrategyPriority, pkgClient.StrategyLeastLoad, pkgClient.StrategyFastest}

// strategyDescriptions provides a brief explanation for each routing strategy.
var strategyDescriptions = map[string]string{
	pkgClient.StrategyPriority:  "Use highest-priority healthy deployment (primary/fallback)",
	pkgClient.StrategyLeastLoad: "Use deployment with fewest active requests (load balancing)",
	pkgClient.StrategyFastest:   "Use deployment with lowest latency (performance)",
}

// ---------------------------------------------------------------------------
// Route form state — embedded in ModelsViewModel
// ---------------------------------------------------------------------------

type routeFormState struct {
	viewport tui.Detail
	// Group form fields
	formName     textinput.Model
	formDesc     textinput.Model
	formStrategy int // index into routeStrategies
	formFocus    routeFormField
	formEditName string // original name when editing

	// Deployment form fields
	deployName       textinput.Model
	deployModelIdx   int // index into availableModels
	deployPriority   textinput.Model
	deployTimeout    textinput.Model
	deployMaxRetries textinput.Model
	deployTags       textinput.Model
	deployFocus      deployFormField

	// Picker data (fetched once, shared across forms)
	availableModels []pkgClient.ModelMetadata
	pickerLoaded    bool

	// Status message
	statusMsg string
}

func newRouteFormState() routeFormState {
	return routeFormState{
		viewport:         tui.NewDetail(),
		formName:         newFormInput("route-name", 64),
		formDesc:         newFormInput("optional description", 128),
		deployName:       newFormInput("deployment-name", 64),
		deployPriority:   newFormInput("1", 8),
		deployTimeout:    newFormInput("30s", 16),
		deployMaxRetries: newFormInput("1", 4),
		deployTags:       newFormInput("tag1, tag2", 128),
	}
}

func newFormInput(placeholder string, charLimit int) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = charLimit
	w := charLimit
	if w > 40 {
		w = 40
	}
	if w < 8 {
		w = 8
	}
	ti.SetWidth(w)
	return ti
}

// ---------------------------------------------------------------------------
// Form: open / focus / tab
// ---------------------------------------------------------------------------

func (f *routeFormState) cancel() {
	f.statusMsg = ""
}

func (f *routeFormState) openNewGroupForm() {
	f.viewport.Reset()
	f.formName.SetValue("")
	f.formDesc.SetValue("")
	f.formStrategy = 0
	f.formFocus = routeFieldName
	f.formEditName = ""
	f.statusMsg = ""
	f.setGroupFormFocus()
}

func (f *routeFormState) openEditGroupForm(g *pkgClient.ModelGroupResponse) {
	f.viewport.Reset()
	f.formEditName = g.Name
	f.formName.SetValue(g.Name)
	f.formDesc.SetValue(g.Description)
	f.formStrategy = 0
	for i, s := range routeStrategies {
		if s == g.Strategy {
			f.formStrategy = i
			break
		}
	}
	f.formFocus = routeFieldName
	f.statusMsg = ""
	f.setGroupFormFocus()
}

func (f *routeFormState) setGroupFormFocus() {
	f.formName.Blur()
	f.formDesc.Blur()

	switch f.formFocus {
	case routeFieldName:
		f.formName.Focus()
	case routeFieldDescription:
		f.formDesc.Focus()
	}
}

func (f *routeFormState) openAddDeployForm(g *pkgClient.ModelGroupResponse) {
	f.viewport.Reset()
	f.deployModelIdx = 0
	// Default priority: next after highest existing (1 if no deployments)
	nextPri := 1
	if g != nil {
		for _, d := range g.Replicas {
			if d.Priority >= nextPri {
				nextPri = d.Priority + 1
			}
		}
	}
	f.deployPriority.SetValue(strconv.Itoa(nextPri))
	f.deployTimeout.SetValue("30s")
	f.deployMaxRetries.SetValue("1")
	f.deployTags.SetValue("")
	f.deployFocus = deployFieldModel
	f.statusMsg = ""
	f.setDeployFormFocus()
}

func (f *routeFormState) setDeployFormFocus() {
	f.deployPriority.Blur()
	f.deployTimeout.Blur()
	f.deployMaxRetries.Blur()
	f.deployTags.Blur()

	switch f.deployFocus {
	case deployFieldPriority:
		f.deployPriority.Focus()
	case deployFieldTimeout:
		f.deployTimeout.Focus()
	case deployFieldMaxRetries:
		f.deployMaxRetries.Focus()
	case deployFieldTags:
		f.deployTags.Focus()
	}
}

// ---------------------------------------------------------------------------
// Form: update handlers
// ---------------------------------------------------------------------------

// Tab order for group form
var groupFormOrder = []routeFormField{
	routeFieldName, routeFieldDescription, routeFieldStrategy,
}

func (f *routeFormState) updateGroupForm(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	// Up/Down and Tab/Shift+Tab navigate between fields
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "tab"))):
		for i, field := range groupFormOrder {
			if field == f.formFocus {
				f.formFocus = groupFormOrder[(i+1)%len(groupFormOrder)]
				break
			}
		}
		f.setGroupFormFocus()
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "shift+tab"))):
		for i, field := range groupFormOrder {
			if field == f.formFocus {
				f.formFocus = groupFormOrder[(i-1+len(groupFormOrder))%len(groupFormOrder)]
				break
			}
		}
		f.setGroupFormFocus()
		return nil

	// Left/Right on strategy field cycles options
	case f.formFocus == routeFieldStrategy &&
		(key.Matches(msg, key.NewBinding(key.WithKeys("left"))) || key.Matches(msg, key.NewBinding(key.WithKeys("right")))):
		if msg.String() == "left" && f.formStrategy > 0 {
			f.formStrategy--
		} else if msg.String() == "right" && f.formStrategy < len(routeStrategies)-1 {
			f.formStrategy++
		}
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return nil // caller handles mode switch

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return nil // caller handles save

	default:
		var cmd tea.Cmd
		switch f.formFocus {
		case routeFieldName:
			f.formName, cmd = f.formName.Update(msg)
		case routeFieldDescription:
			f.formDesc, cmd = f.formDesc.Update(msg)
		}
		return cmd
	}
}

// Tab order for deploy form
var deployFormOrder = []deployFormField{
	deployFieldModel, deployFieldPriority,
	deployFieldTimeout, deployFieldMaxRetries, deployFieldTags,
}

func (f *routeFormState) updateDeployForm(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		for i, field := range deployFormOrder {
			if field == f.deployFocus {
				f.deployFocus = deployFormOrder[(i+1)%len(deployFormOrder)]
				break
			}
		}
		f.setDeployFormFocus()
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		for i, field := range deployFormOrder {
			if field == f.deployFocus {
				f.deployFocus = deployFormOrder[(i-1+len(deployFormOrder))%len(deployFormOrder)]
				break
			}
		}
		f.setDeployFormFocus()
		return nil

	case f.deployFocus == deployFieldModel &&
		(key.Matches(msg, key.NewBinding(key.WithKeys("left"))) || key.Matches(msg, key.NewBinding(key.WithKeys("right")))):
		isLeft := msg.String() == "left"
		if len(f.availableModels) > 0 {
			if isLeft && f.deployModelIdx > 0 {
				f.deployModelIdx--
			} else if !isLeft && f.deployModelIdx < len(f.availableModels)-1 {
				f.deployModelIdx++
			}
		}
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return nil // caller handles mode switch

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return nil // caller handles save

	default:
		var cmd tea.Cmd
		switch f.deployFocus {
		case deployFieldPriority:
			f.deployPriority, cmd = f.deployPriority.Update(msg)
		case deployFieldTimeout:
			f.deployTimeout, cmd = f.deployTimeout.Update(msg)
		case deployFieldMaxRetries:
			f.deployMaxRetries, cmd = f.deployMaxRetries.Update(msg)
		case deployFieldTags:
			f.deployTags, cmd = f.deployTags.Update(msg)
		}
		return cmd
	}
}

// ---------------------------------------------------------------------------
// Form: save logic
// ---------------------------------------------------------------------------

func (f *routeFormState) buildGroupRequest(existingGroup *pkgClient.ModelGroupResponse) (string, *pkgClient.CreateOrUpdateModelGroupRequest) {
	name := strings.TrimSpace(f.formName.Value())
	if name == "" {
		f.statusMsg = "Name is required"
		return "", nil
	}
	if strings.ContainsAny(name, "/?#") {
		f.statusMsg = "Name cannot contain /, ?, or #"
		return "", nil
	}

	req := &pkgClient.CreateOrUpdateModelGroupRequest{
		Description: strings.TrimSpace(f.formDesc.Value()),
		Strategy:    routeStrategies[f.formStrategy],
	}

	// Preserve existing deployments and health check when editing
	if existingGroup != nil {
		existing := groupToRequest(existingGroup, -1)
		req.Replicas = existing.Replicas
		req.HealthCheck = existing.HealthCheck
	}

	return name, req
}

func (f *routeFormState) buildDeployRequest(g *pkgClient.ModelGroupResponse) (string, *pkgClient.CreateOrUpdateModelGroupRequest) {
	if g == nil {
		return "", nil
	}

	if len(f.availableModels) == 0 {
		f.statusMsg = "No models available: deploy a model first"
		return "", nil
	}
	selected := f.availableModels[f.deployModelIdx]
	priority, err := strconv.Atoi(strings.TrimSpace(f.deployPriority.Value()))
	if err != nil {
		f.statusMsg = "Priority must be a number"
		return "", nil
	}
	// An empty field leaves max_retries unset (the server default applies).
	var maxRetries *int
	if s := strings.TrimSpace(f.deployMaxRetries.Value()); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			f.statusMsg = "Max retries must be a number"
			return "", nil
		}
		maxRetries = &n
	}

	var tags []string
	tagsStr := strings.TrimSpace(f.deployTags.Value())
	if tagsStr != "" {
		for t := range strings.SplitSeq(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	newDep := pkgClient.ReplicaRequest{
		Name:       selected.Name,
		Model:      selected.Name,
		App:        selected.AssignedApp,
		Node:       selected.Node,
		Priority:   priority,
		Timeout:    strings.TrimSpace(f.deployTimeout.Value()),
		MaxRetries: maxRetries,
		Tags:       tags,
	}

	req := groupToRequest(g, -1)
	req.Replicas = append(req.Replicas, newDep)

	return g.Name, req
}

// ---------------------------------------------------------------------------
// Form: overlay rendering
// ---------------------------------------------------------------------------

func renderRouteFormOverlay(f *routeFormState, styles ui.Styles, bg string, width, height int, title string) string {
	var popup strings.Builder
	focusLine := 0
	f.formName.SetWidth(max(form.OverlayWidth(width)-22, 1))
	f.formDesc.SetWidth(max(form.OverlayWidth(width)-22, 1))

	popup.WriteString(styles.Title.Render(title) + "\n\n")

	focusPrefix := func(field routeFormField) string {
		if f.formFocus == field {
			focusLine = strings.Count(popup.String(), "\n")
			return "▸ "
		}
		return "  "
	}

	popup.WriteString(styles.Normal.Render(focusPrefix(routeFieldName)+"Name:         ") + f.formName.View() + "\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(routeFieldDescription)+"Description:  ") + f.formDesc.View() + "\n")

	// Strategy selector with full description
	popup.WriteString("\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(routeFieldStrategy)+"Strategy:     ") + "‹ " + routeStrategies[f.formStrategy] + " ›\n")
	if desc, ok := strategyDescriptions[routeStrategies[f.formStrategy]]; ok {
		popup.WriteString(styles.Help.Render("                "+desc) + "\n")
	}

	if f.statusMsg != "" {
		popup.WriteString("\n" + styles.Error.Render(f.statusMsg) + "\n")
	}

	return tui.RenderForm(styles, &f.viewport, bg, popup.String(), "Ctrl+S save  Tab next  ←/→ choose  Esc cancel", width, height, focusLine)
}

func renderDeployFormOverlay(f *routeFormState, styles ui.Styles, bg string, width, height int, groupName string) string {
	var popup strings.Builder
	focusLine := 0
	for _, input := range []*textinput.Model{&f.deployPriority, &f.deployTimeout, &f.deployMaxRetries, &f.deployTags} {
		input.SetWidth(max(form.OverlayWidth(width)-22, 1))
	}

	title := "Add Deployment"
	if groupName != "" {
		title = fmt.Sprintf("Add Deployment to: %s", groupName)
	}
	popup.WriteString(styles.Title.Render(title) + "\n\n")

	focusPrefix := func(field deployFormField) string {
		if f.deployFocus == field {
			focusLine = strings.Count(popup.String(), "\n")
			return "▸ "
		}
		return "  "
	}

	modelDisplay := "(no models: deploy a model first)"
	if len(f.availableModels) > 0 {
		modelDisplay = renderModelPicker(f.availableModels, f.deployModelIdx)
	}
	popup.WriteString(styles.Normal.Render(focusPrefix(deployFieldModel)+"Model:        ") + modelDisplay + "\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(deployFieldPriority)+"Priority:     ") + f.deployPriority.View() + "\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(deployFieldTimeout)+"Timeout:      ") + f.deployTimeout.View() + "\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(deployFieldMaxRetries)+"Max Retries:  ") + f.deployMaxRetries.View() + "\n")
	popup.WriteString(styles.Normal.Render(focusPrefix(deployFieldTags)+"Tags:         ") + f.deployTags.View() + "\n")

	if f.statusMsg != "" {
		popup.WriteString("\n" + styles.Error.Render(f.statusMsg) + "\n")
	}

	return tui.RenderForm(styles, &f.viewport, bg, popup.String(), "Ctrl+S save  Tab next  Esc cancel", width, height, focusLine)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// isAutoRoute returns true if the group was auto-created by the auto-route manager.
func isAutoRoute(g *pkgClient.ModelGroupResponse) bool {
	return g != nil && g.AutoManaged
}

const statusOnline = "online"

// deploymentStatus computes the display status string for a deployment.
func replicaStatus(d *pkgClient.ReplicaResponse, nodeHealth map[string]string) string {
	if d.CooldownSeconds > 0 {
		reason := cooldownReasonLabel(d.CooldownReason)
		return fmt.Sprintf("%s (%.0fs)", reason, d.CooldownSeconds)
	}
	if d.Node != "" {
		if s, ok := nodeHealth[d.Node]; ok {
			return s
		}
	}
	return statusOnline
}

// statusDot returns a filled or empty dot for the given status.
func statusDot(status string) string {
	if status == statusOnline {
		return "●"
	}
	return "○"
}

// cooldownReasonLabel converts a cooldown reason code to a human-readable label.
func cooldownReasonLabel(reason string) string {
	switch reason {
	case "rate_limit":
		return "rate limited"
	case "quota":
		return "quota exhausted"
	case "unavailable":
		return "unavailable"
	case "transport":
		return "unreachable"
	default:
		return "cooldown"
	}
}

// groupToRequest builds a CreateOrUpdateModelGroupRequest from an existing group response,
// optionally skipping a deployment by index (-1 to keep all).
func groupToRequest(g *pkgClient.ModelGroupResponse, skipDeployIdx int) *pkgClient.CreateOrUpdateModelGroupRequest {
	req := &pkgClient.CreateOrUpdateModelGroupRequest{
		Description: g.Description,
		Strategy:    g.Strategy,
	}
	if g.HealthCheck != nil {
		req.HealthCheck = &pkgClient.HealthCheckConfigRequest{
			Path:     g.HealthCheck.Path,
			Interval: g.HealthCheck.Interval,
			Timeout:  g.HealthCheck.Timeout,
		}
	}
	for i, d := range g.Replicas {
		if i == skipDeployIdx {
			continue
		}
		req.Replicas = append(req.Replicas, pkgClient.ReplicaRequest{
			Name:              d.Name,
			Model:             d.Model,
			App:               d.App,
			Node:              d.Node,
			Priority:          d.Priority,
			Timeout:           d.Timeout,
			OnDemand:          d.OnDemand,
			GPUMemoryRequired: d.GPUMemoryRequired,
			MaxRetries:        d.MaxRetries,
			Tags:              d.Tags,
		})
	}
	return req
}

// renderModelPicker renders a ←/→ cycle picker showing model name with app and node.
func renderModelPicker(models []pkgClient.ModelMetadata, selected int) string {
	if len(models) == 0 {
		return "(none)"
	}
	if selected < 0 || selected >= len(models) {
		selected = 0
	}
	m := models[selected]
	label := m.Name
	if m.AssignedApp != "" {
		label += " (" + m.AssignedApp
		if m.Node != "" {
			label += " @ " + m.Node
		}
		label += ")"
	}

	left := "  "
	right := "  "
	if selected > 0 {
		left = "← "
	}
	if selected < len(models)-1 {
		right = " →"
	}
	return left + label + right
}

func (f *routeFormState) values() string {
	return fmt.Sprint(f.formName.Value(), "\x00", f.formDesc.Value(), "\x00", f.formStrategy, "\x00", f.deployName.Value(), "\x00", f.deployPriority.Value(), "\x00", f.deployTimeout.Value(), "\x00", f.deployMaxRetries.Value(), "\x00", f.deployTags.Value(), "\x00", f.deployModelIdx)
}
