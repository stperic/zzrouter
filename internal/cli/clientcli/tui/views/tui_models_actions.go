package views

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func (m *ModelsViewModel) actionStartStop() tea.Cmd {
	r := m.selectedRow()
	if r == nil {
		return nil
	}

	if r.instance != nil {
		// Running — confirm stop
		m.confirmKind = confirmStopModel
		m.confirmMessage = fmt.Sprintf("Stop %s on %s? (y/n)", r.RawModel, r.Node)
		return nil
	}

	if r.sortPriority == 2 {
		// Idle — start the model
		client := m.client
		node := r.Node
		model := r.RawModel
		provider := r.Provider
		return func() tea.Msg {
			_, err := client.LoadModelWithProviderForceParamsAndEnv(node, model, provider, false, nil, nil)
			return modelsActionMsg{action: "start", err: err}
		}
	}

	return nil
}

// actionDelete asks for confirmation before deleting
func (m *ModelsViewModel) actionDelete() tea.Cmd {
	r := m.selectedRow()
	if r == nil {
		return nil
	}

	if r.deploy != nil {
		// In-flight transfer — the actionable verb is cancel, not delete.
		m.confirmKind = confirmDeleteDownload
		m.confirmMessage = fmt.Sprintf("Cancel the in-flight download of %s on %s? (y/n)",
			r.RawModel, r.deploy.Node)
		return nil
	}

	m.confirmKind = confirmDeleteModel
	if r.instance != nil && r.instance.Status == "running" {
		m.confirmMessage = fmt.Sprintf("Stop and delete %s from %s? (y/n)", r.RawModel, r.Node)
	} else {
		m.confirmMessage = fmt.Sprintf("Delete %s from %s? (y/n)", r.RawModel, r.Node)
	}
	return nil
}

// executeConfirmedAction runs the confirmed action
func (m *ModelsViewModel) executeConfirmedAction() tea.Cmd {
	r := m.selectedRow()
	if r == nil {
		m.confirmKind = confirmNone
		return nil
	}

	client := m.client
	action := m.confirmKind

	switch action {
	case confirmStopModel:
		if r.instance != nil {
			r.Status = "stopping"
			inst := r.instance
			return func() tea.Msg {
				err := client.StopInstance(inst.ID, inst.Node)
				return modelsActionMsg{action: "stop", err: err}
			}
		}
	case confirmDeleteModel:
		if r.instance != nil {
			inst := r.instance
			reg := r.registry
			return func() tea.Msg {
				if err := client.StopInstance(inst.ID, inst.Node); err != nil {
					return modelsActionMsg{action: "delete", err: fmt.Errorf("failed to stop before delete: %w", err)}
				}
				if reg != nil {
					regCopy := *reg
					if _, err := client.DeleteModelsFromRegistry([]pkgClient.ModelMetadata{regCopy}); err != nil {
						return modelsActionMsg{action: "delete", err: err}
					}
				}
				return modelsActionMsg{action: "delete", err: nil}
			}
		} else if r.registry != nil {
			reg := *r.registry
			return func() tea.Msg {
				_, err := client.DeleteModelsFromRegistry([]pkgClient.ModelMetadata{reg})
				return modelsActionMsg{action: "delete", err: err}
			}
		}
	case confirmDeleteDownload:
		if r.deploy != nil {
			// Cancel through the deployment, not the job. The download
			// goroutine is bound to the tracker's cancel func and never
			// watches its job context, so DELETE /jobs/:id returns 202 and
			// the transfer keeps going.
			depID, node := r.deploy.DeploymentID, r.deploy.Node
			if depID == "" || node == "" {
				// Nothing addressable: the transfer already finished, or the
				// record outlived the deployment. Saying so beats dropping
				// the row and letting the next poll bring it straight back.
				return func() tea.Msg {
					return modelsActionMsg{action: "cancel",
						err: fmt.Errorf("this download is no longer cancellable")}
				}
			}
			return func() tea.Msg {
				err := client.CancelDeploymentNode(depID, node)
				return modelsActionMsg{action: "cancel", err: err}
			}
		}
	}

	m.confirmKind = confirmNone
	return nil
}

func (m *ModelsViewModel) tickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return modelsTickMsg(t)
	})
}

func (m *ModelsViewModel) autoRefreshCmd() tea.Cmd {
	epoch := m.pollEpoch
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg {
		return modelsAutoRefreshMsg{epoch: epoch}
	})
}
