package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

func hasInstallPatch(patch *paramPatchBody, cfg config.ServiceConfig) bool {
	if patch.Defaults != nil && len(patch.Defaults.Install) > 0 {
		return true
	}
	for name, node := range patch.Nodes {
		if node == nil && cfg.Nodes[name].Install != nil || node != nil && len(node.Install) > 0 {
			return true
		}
	}
	return false
}

func validateInstallPatch(patch *paramPatchBody, cfg config.ServiceConfig, restart bool) []utils.ParamError {
	var errs []utils.ParamError
	if patch.Defaults != nil && len(patch.Defaults.Install) > 0 {
		errs = append(errs, installPatchShape(patch.Defaults.Install, "defaults.install")...)
	}
	for name, node := range patch.Nodes {
		if node != nil && len(node.Install) > 0 {
			errs = append(errs, installPatchShape(node.Install, "nodes."+name+".install")...)
		}
	}
	if restart && hasInstallPatch(patch, cfg) {
		errs = append(errs, utils.ParamError{Key: "install", Code: string(httperr.CodeInvalidValue), Message: "install changes cannot use restart=affected"})
	}
	return errs
}

func installPatchShape(raw json.RawMessage, path string) []utils.ParamError {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return []utils.ParamError{{Key: path, Code: string(httperr.CodeWrongType), Message: err.Error()}}
	}
	return checkInstallShape(value, reflect.TypeFor[config.InstallConfig](), path)
}

func checkInstallShape(value any, typ reflect.Type, path string) []utils.ParamError {
	if value == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	fail := func(code httperr.ParamErrorCode, message string) []utils.ParamError {
		return []utils.ParamError{{Key: path, Code: string(code), Message: message}}
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fail(httperr.CodeWrongType, "object required")
		}
		fields := map[string]reflect.Type{}
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			fields[name] = field.Type
		}
		result := []utils.ParamError{}
		for name, leaf := range object {
			field, ok := fields[name]
			if !ok {
				result = append(result, utils.ParamError{Key: path + "." + name, Code: string(httperr.CodeUnknownField), Message: "unknown install field"})
				continue
			}
			result = append(result, checkInstallShape(leaf, field, path+"."+name)...)
		}
		return result
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return fail(httperr.CodeWrongType, "object required")
		}
		result := []utils.ParamError{}
		for name, leaf := range object {
			if !config.ValidDistribution(name) {
				result = append(result, utils.ParamError{Key: path + "." + name, Code: string(httperr.CodeInvalidValue), Message: "normalized runtime or package name required"})
			}
			result = append(result, checkInstallShape(leaf, typ.Elem(), path+"."+name)...)
		}
		return result
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return fail(httperr.CodeWrongType, "array required")
		}
		if len(array) > 32 {
			return fail(httperr.CodeOutOfRange, "array exceeds 32 values")
		}
		result := []utils.ParamError{}
		for index, leaf := range array {
			result = append(result, checkInstallShape(leaf, typ.Elem(), fmt.Sprintf("%s[%d]", path, index))...)
		}
		return result
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return fail(httperr.CodeWrongType, "boolean required")
		}
	case reflect.String:
		text, ok := value.(string)
		if !ok {
			return fail(httperr.CodeWrongType, "string required")
		}
		var err error
		switch {
		case strings.HasSuffix(path, ".package"):
			if !config.ValidDistribution(text) {
				err = fmt.Errorf("normalized package required")
			}
		case strings.HasSuffix(path, ".constraint") || strings.HasSuffix(path, ".version_constraint"):
			err = config.ValidateSpecifier(text)
		case strings.Contains(path, ".imports[") || strings.HasSuffix(path, ".startup_import"):
			if !config.ValidInstallModule(text) {
				err = fmt.Errorf("dotted Python module required")
			}
		case strings.HasSuffix(path, ".primary") || strings.Contains(path, ".extra["):
			err = config.ValidateInstallIndex(text)
		}
		if err != nil {
			return fail(httperr.CodeInvalidValue, err.Error())
		}
	}
	return nil
}

func cloneService(sc *config.ServiceConfig) (config.ServiceConfig, error) {
	data, err := yaml.Marshal(sc)
	if err != nil {
		return config.ServiceConfig{}, err
	}
	var copy config.ServiceConfig
	err = yaml.Unmarshal(data, &copy)
	return copy, err
}

func (e *ParamsExecutor) checkInstallMutation(ctx context.Context, before *config.ServiceConfig, after *config.ServiceConfig, nodes map[string]bool) error {
	if err := after.ValidateInstall(); err != nil {
		return paramErrors{{Key: "install", Code: string(httperr.CodeInvalidValue), Message: err.Error()}}
	}
	if after.Install == nil {
		return nil
	}
	for node := range nodes {
		affected := false
		for runtime := range after.Install.Runtimes {
			old, oldErr := before.ResolveInstall(node, runtime)
			next, err := after.ResolveInstall(node, runtime)
			if err != nil {
				return err
			}
			if oldErr != nil || install.Fingerprint(old) != install.Fingerprint(next) {
				affected = true
			}
		}
		if !affected {
			continue
		}
		if e.installAuthority == nil {
			return paramErrors{{Key: "nodes." + node + ".install", Code: string(httperr.CodePreflightFailed), Message: "node install authority unavailable"}}
		}
		if err := e.installAuthority(ctx, node, *after); err != nil {
			return paramErrors{{Key: "nodes." + node + ".install", Code: string(httperr.CodePreflightFailed), Message: err.Error()}}
		}
	}
	return nil
}

type installAuthorityRequest struct {
	Install  *config.InstallConfig            `json:"install"`
	Defaults *config.InstallConfig            `json:"defaults"`
	Nodes    map[string]*config.InstallConfig `json:"nodes"`
}

// HandleInternalInstallAuthority checks proposed data without storing it or executing imports.
func (e *ProvidersExecutor) HandleInternalInstallAuthority(c *gin.Context) {
	if e.appsConfig == nil || e.appsConfig() == nil || e.mgr == nil {
		InternalNodeError(c, "install authority unavailable")
		return
	}
	cfg, ok := e.appsConfig().LookupApp(c.Param("name"))
	if !ok {
		NotFound(c, "provider not found")
		return
	}
	var proposed installAuthorityRequest
	if !BindJSONStrict(c, &proposed) {
		return
	}
	cfg.Install = proposed.Install
	if cfg.Defaults == nil {
		cfg.Defaults = &config.AppDefaultsConfig{}
	}
	// Work on detached data rather than the manager's published pointer fields.
	copy, err := cloneService(&cfg)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	cfg = copy
	cfg.Defaults.Install = proposed.Defaults
	for node, cell := range cfg.Nodes {
		cell.Install = nil
		cfg.Nodes[node] = cell
	}
	for node, override := range proposed.Nodes {
		cell := cfg.Nodes[node]
		cell.Install = override
		if cfg.Nodes == nil {
			cfg.Nodes = map[string]config.NodeSpec{}
		}
		cfg.Nodes[node] = cell
	}
	if err := cfg.ValidateInstall(); err != nil {
		BadRequest(c, err.Error())
		return
	}
	fingerprints := map[string]string{}
	if cfg.Install == nil {
		BadRequest(c, "provider has no managed recipe")
		return
	}
	for runtime := range cfg.Install.Runtimes {
		snapshot, err := e.mgr.CheckInstallAuthority(cfg, runtime)
		if err != nil {
			respondRecipeErr(c, err)
			return
		}
		fingerprints[runtime] = snapshot.PolicyFingerprint
	}
	c.JSON(http.StatusOK, gin.H{"node": e.nodeName, "policy_fingerprints": fingerprints})
}

func (s *Server) checkNodeInstallAuthority(ctx context.Context, node string, sc config.ServiceConfig) error {
	if node == s.node.Nodename() {
		if s.providers.appMgr == nil {
			return fmt.Errorf("install authority unavailable")
		}
		for runtime := range sc.Install.Runtimes {
			if _, err := s.providers.appMgr.CheckInstallAuthority(sc, runtime); err != nil {
				return err
			}
		}
		return nil
	}
	body := installAuthorityRequest{Install: sc.Install, Nodes: map[string]*config.InstallConfig{}}
	if sc.Defaults != nil {
		body.Defaults = sc.Defaults.Install
	}
	for name, cell := range sc.Nodes {
		body.Nodes[name] = cell.Install
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := s.cluster.router.Route(ctx, &routing.Request{Method: http.MethodPost, Path: "/zzrouter/v1/internal/providers/" + url.PathEscape(sc.Name) + "/install/authority", Node: node, Body: data})
	if err != nil {
		return fmt.Errorf("target node authority not acknowledged: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("target node refused install authority: HTTP %d", response.StatusCode)
	}
	return nil
}
