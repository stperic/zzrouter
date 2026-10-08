package server

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

const environmentContract = "runtime_environment_v1"

type providerEnvironment struct {
	DesiredInstall    *pkgConfig.ResolvedInstall  `json:"desired_install,omitempty"`
	InstalledManifest *fsroot.InstallManifest     `json:"installed_manifest,omitempty"`
	RecipeDrift       bool                        `json:"recipe_drift"`
	InstallAuthority  *install.PolicyCapabilities `json:"install_authority,omitempty"`
	Contract          string                      `json:"contract"`
	Provider          string                      `json:"provider"`
	Runtime           string                      `json:"runtime"`
	Node              string                      `json:"node"`
	Installed         bool                        `json:"installed"`
	Drivers           []gpu.Card                  `json:"drivers"`
	DriverEvidence    string                      `json:"driver_evidence"`
	Environment       *install.RuntimeEnvironment `json:"environment,omitempty"`
	Kernels           string                      `json:"kernels"`
	KernelEvidence    string                      `json:"kernel_evidence"`
	Findings          []install.HostFinding       `json:"findings"`
	Workarounds       []environmentWorkaround     `json:"api_workarounds"`
}

type environmentWorkaround struct {
	Variable string         `json:"variable"`
	Value    string         `json:"value"`
	Active   bool           `json:"active"`
	Method   string         `json:"method"`
	Path     string         `json:"path"`
	Body     map[string]any `json:"body"`
}

func environmentWorkarounds(provider, node string, checks schema.RuntimeChecks, environment map[string]string) []environmentWorkaround {
	result := make([]environmentWorkaround, 0, len(checks.Workaround))
	for key, value := range checks.Workaround {
		result = append(result, environmentWorkaround{
			Variable: key, Value: value, Active: environment[key] == value,
			Method: "PATCH", Path: "/zzrouter/v1/providers/" + url.PathEscape(provider) + "/parameters",
			Body: map[string]any{"nodes": map[string]any{node: map[string]any{"environment": map[string]string{key: value}}}},
		})
	}
	return result
}

// GetProviderEnvironment reports prerequisites on the selected node without changing it.
func (ctrl *ProvidersController) GetProviderEnvironment(c *gin.Context) {
	result, err := ctrl.providersService.GetProviderEnvironment(c.Request.Context(), c.Param("name"), c.Query("runtime"), QueryNode(c))
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Provider environment observed", result)
}

// HandleInternalProviderEnvironment probes only this node's managed runtime.
func (e *ProvidersExecutor) HandleInternalProviderEnvironment(c *gin.Context) {
	name := c.Param("name")
	if e.configStore == nil || e.appsConfig == nil || e.mgr == nil {
		InternalNodeError(c, "provider diagnostics unavailable on this node")
		return
	}
	apps := e.appsConfig()
	if apps == nil {
		InternalNodeError(c, "provider configuration unavailable")
		return
	}
	cfg, ok := apps.LookupApp(name)
	if !ok {
		NotFound(c, "provider not found")
		return
	}
	data, err := e.configStore.ReadProviderSchemaBytes(name)
	if err != nil {
		NotFound(c, "provider schema unavailable")
		return
	}
	declared, err := schema.LoadYAMLSchema(data)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	if declared.Diagnostics == nil {
		BadRequest(c, "runtime diagnostics unsupported on this node; upgrade the node")
		return
	}
	if err := declared.Diagnostics.Validate(); err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	runtime := c.Query("runtime")
	if runtime == "" {
		runtime = declared.Diagnostics.Runtime
	}
	checks, ok := declared.Diagnostics.Runtimes[runtime]
	if !ok {
		BadRequest(c, "runtime not declared by this provider")
		return
	}
	inst, err := e.mgr.Install().Installer(runtime)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	ctx, cancel := ensureTimeout(c.Request.Context(), LongRequestTimeout)
	defer cancel()
	result := providerEnvironment{
		Contract: environmentContract, Provider: name, Runtime: runtime, Node: e.nodeName,
		Installed: inst.IsInstalled(), Drivers: gpu.List(false).Cards,
		DriverEvidence: "Cached node hardware inventory; missing values are unknown. cuda_version is the driver maximum, not the loaded runtime.",
		Kernels:        checks.Kernels, KernelEvidence: "Schema declares kernel capability. Package presence does not establish which kernel path a launch executes; this read-only probe does not compile kernels or load model weights.",
		Findings: []install.HostFinding{},
	}
	environment := pkgConfig.FlattenEnvironment(cfg.Resolve(e.nodeName, "").Environment)
	if environment == nil {
		environment = map[string]string{}
	}
	var toolkit install.ToolkitSelection
	if cfg.Install != nil {
		desired, err := cfg.ResolveInstall(e.nodeName, runtime)
		if err != nil {
			BadRequest(c, err.Error())
			return
		}
		result.DesiredInstall = &desired
		capabilities := e.mgr.InstallPolicyCapabilities(cfg, runtime)
		result.InstallAuthority = &capabilities
		manifest, err := fsroot.ReadInstallManifest(runtime)
		if err != nil {
			InternalNodeError(c, err.Error())
			return
		}
		result.InstalledManifest = manifest
		if manifest != nil {
			result.RecipeDrift = manifest.RecipeFingerprint != install.Fingerprint(desired.Recipe)
		} else {
			result.RecipeDrift = result.Installed
		}
		if manifest != nil && manifest.Recipe != nil {
			checks = (install.RecipeSnapshot{ResolvedInstall: pkgConfig.ResolvedInstall{Recipe: *manifest.Recipe}}).RuntimeChecks(checks)
			toolkit, err = install.ApplyToolkitEnvironment(ctx, environment, runtime)
			if err != nil {
				result.Findings = append(result.Findings, install.HostFinding{Name: "managed_toolkit", Detected: err.Error(), Required: "Intact compiler inside the managed runtime", Impact: "Launch refuses host toolkit fallback.", APIWorkarounds: map[string]string{"repair": "POST install with force:true"}})
			}
		}
	}
	result.Workarounds = environmentWorkarounds(name, e.nodeName, checks, environment)
	if result.Drivers == nil {
		result.Drivers = []gpu.Card{}
	}
	if python, ok := inst.(install.ManagedInterpreter); ok && result.Installed {
		snapshot, authorityErr := e.installedRecipeAuthority(name, runtime, result.InstalledManifest)
		if authorityErr == nil {
			authorityErr = install.CheckRecipeAuthority(ctx, snapshot, nil)
		}
		if authorityErr != nil {
			result.Findings = append(result.Findings, install.HostFinding{Name: "installed_recipe_authority", Detected: authorityErr.Error(), Required: "Current protected authority for the installed recipe", Impact: "Installed modules were not imported.", HumanRemediation: []string{"Have the operator approve the installed recipe or reinstall the unchanged release recipe."}})
			c.JSON(http.StatusOK, result)
			return
		}
		checks = snapshot.RuntimeChecks(checks)
		result.Environment, err = install.InspectRuntime(ctx, python.PythonPath(), checks, environment, toolkit)
		if err != nil {
			InternalNodeError(c, fmt.Sprintf("observe runtime: %v", err))
			return
		}
		capabilities := make([]string, 0, len(result.Drivers))
		for _, device := range result.Environment.Devices {
			capabilities = append(capabilities, device.Capability)
		}
		result.Findings = append(result.Findings, install.ToolkitFindings(checks, result.Environment.Toolkit, capabilities)...)
		result.Findings = append(result.Findings, install.CompilerFindings(checks, result.Environment.CPPCompiler)...)
	}
	c.JSON(http.StatusOK, result)
}
