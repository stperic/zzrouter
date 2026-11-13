package clientcli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func TestHostNavigationDoesNotRetainDirectDeployLoading(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic failure", http.StatusInternalServerError)
	}))
	defer server.Close()
	c := client.NewClient(config.ClientNodeConfig{Address: server.URL})
	styles := ui.NewStyles(ui.CatppuccinMocha())
	model := &tuiModel{
		client: c, styles: styles, root: tui.NewEmptyRoot(),
		search: search.NewSearchTUIModel(c, styles, "", "huggingface", "", "", "", true),
	}
	defer model.search.Deactivate()
	model.enterMenuItem(tuiViewSearch)
	model.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	for _, ch := range "org/model" {
		model.Update(tea.KeyPressMsg{Code: ch})
	}
	_, validation := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, validation)
	model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2})
	require.Equal(t, paneMenu, model.pane, "actual breadcrumb handling left Search")
	model.Update(validation())
	model.enterMenuItem(tuiViewSearch)
	model.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	require.Contains(t, model.search.Breadcrumb(), "Direct Deploy")
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotContains(t, model.search.Breadcrumb(), "Direct Deploy", "Escape remains available after host navigation and reentry")
}
