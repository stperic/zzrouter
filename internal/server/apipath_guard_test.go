package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// The Go client's view of the API must match the routes the server
// registers.
//
// This is the check that did not exist. The client restated every path
// as a string literal and nothing compared them to engine.Routes(), so
// renaming a route compiled clean and broke the CLI at runtime, on
// whichever command someone ran next. pkg/apipath centralises the paths;
// this test is what makes centralising them worth anything.
//
// It lives in internal/server rather than beside apipath because it
// needs a built engine, and apipath must not import the server.

// apipathSamples is every exported path in pkg/apipath, with sample
// arguments for the builders. Go cannot enumerate a package's members at
// runtime, so this list is written out — and
// TestAPIPathSamples_CoverEveryExportedPath below AST-parses apipath and
// fails if anything here is missing. Declared list plus completeness
// guard: neither half is trusted alone.
func apipathSamples() map[string]string {
	const (
		id   = "sample-id"
		name = "sample-name"
		key  = "sample-key"
	)
	return map[string]string{
		"Health":          apipath.Health,
		"ServerVersion":   apipath.ServerVersion,
		"Search":          apipath.Search,
		"SpendReport":     apipath.SpendReport,
		"ProvidersStatus": apipath.ProvidersStatus,
		"ServerRoutes":    apipath.ServerRoutes,

		"Keys":          apipath.Keys,
		"Teams":         apipath.Teams,
		"Runs":          apipath.Runs,
		"Jobs":          apipath.Jobs,
		"Models":        apipath.Models,
		"Nodes":         apipath.Nodes,
		"Providers":     apipath.Providers,
		"Deployments":   apipath.Deployments,
		"ModelGroups":   apipath.ModelGroups,
		"InferenceLogs": apipath.InferenceLogs,
		"UsageModels":   apipath.UsageModels,

		"ModelsDefaults":                apipath.ModelsDefaults,
		"ModelsShow":                    apipath.ModelsShow,
		"NodesCompatible":               apipath.NodesCompatible,
		"ProvidersCatalog":              apipath.ProvidersCatalog,
		"ProvidersInstances":            apipath.ProvidersInstances,
		"RunsLoad":                      apipath.RunsLoad,
		"RunsPreview":                   apipath.RunsPreview,
		"PricingOverrides":              apipath.PricingOverrides,
		"PricingStatus":                 apipath.PricingStatus,
		"UpdateStatus":                  apipath.UpdateStatus,
		"UpdateCheck":                   apipath.UpdateCheck,
		"UpdateApply":                   apipath.UpdateApply,
		"UpdateRollback":                apipath.UpdateRollback,
		"UpdateHistory":                 apipath.UpdateHistory,
		"ClusterPairingAccept":          apipath.ClusterPairingAccept,
		"RegistriesHuggingFaceVariants": apipath.RegistriesHuggingFaceVariants,

		"Key":            apipath.Key(id),
		"KeyRotate":      apipath.KeyRotate(id),
		"KeyUsage":       apipath.KeyUsage(id),
		"KeyUsageReset":  apipath.KeyUsageReset(id),
		"Team":           apipath.Team(id),
		"TeamKeys":       apipath.TeamKeys(id),
		"TeamUsage":      apipath.TeamUsage(id),
		"TeamUsageReset": apipath.TeamUsageReset(id),

		"Run":       apipath.Run(id),
		"RunLogs":   apipath.RunLogs(id),
		"Job":       apipath.Job(id),
		"JobStream": apipath.JobStream(id),
		"Node":      apipath.Node(name),

		"Deployment":     apipath.Deployment(id),
		"DeploymentNode": apipath.DeploymentNode(id, name),

		"ModelGroup":          apipath.ModelGroup(name),
		"InferenceLog":        apipath.InferenceLog(id),
		"InferenceLogPayload": apipath.InferenceLogPayload(id),
		"UsageModel":          apipath.UsageModel(name),
		"ModelCard":           apipath.ModelCard("hf", "org/model"),

		"ClusterEndpoint": apipath.ClusterEndpoint("10.0.0.1:9090"),
		"RunProbes":       apipath.RunProbes(id),

		"Provider":                   apipath.Provider(name),
		"ProviderVersionsFor":        apipath.ProviderVersionsFor(name),
		"ProviderSchema":             apipath.ProviderSchema(name),
		"ProviderResolved":           apipath.ProviderResolved(name),
		"ProviderAssets":             apipath.ProviderAssets(name),
		"ProviderAsset":              apipath.ProviderAsset(name, "sample.jinja"),
		"ProviderServiceStatus":      apipath.ProviderServiceStatus(name),
		"ProviderServiceApply":       apipath.ProviderServiceApply(name),
		"ProviderUpgrade":            apipath.ProviderUpgrade(name),
		"ProviderVerify":             apipath.ProviderVerify(name),
		"ProviderInstall":            apipath.ProviderInstall(name),
		"ProviderInstallPlan":        apipath.ProviderInstallPlan(name),
		"ProviderInstallPreflight":   apipath.ProviderInstallPreflight(name),
		"ProviderInstallVerifyStep":  apipath.ProviderInstallVerifyStep(name),
		"ProviderInstallExecuteStep": apipath.ProviderInstallExecuteStep(name),

		"ProviderParameters":           apipath.ProviderParameters(name),
		"ProviderParameter":            apipath.ProviderParameter(name, key),
		"ProviderParameterIgnore":      apipath.ProviderParameterIgnore(name, key),
		"ProviderNodesParameters":      apipath.ProviderNodesParameters(name),
		"ProviderNodeParameter":        apipath.ProviderNodeParameter(name, key),
		"ProviderNodeParameterIgnore":  apipath.ProviderNodeParameterIgnore(name, key),
		"ProviderModelsParameters":     apipath.ProviderModelsParameters(name),
		"ProviderModelParameter":       apipath.ProviderModelParameter(name, key),
		"ProviderModelParameterIgnore": apipath.ProviderModelParameterIgnore(name, key),
	}
}

func TestAPIPaths_AllResolveToRegisteredRoutes(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	templates := make([]string, 0, 256)
	seen := map[string]bool{}
	for _, r := range s.engine.Routes() {
		if !seen[r.Path] {
			seen[r.Path] = true
			templates = append(templates, r.Path)
		}
	}
	if len(templates) == 0 {
		t.Fatal("engine registered no routes; the guard would pass vacuously")
	}

	var unmatched []string
	for name, path := range apipathSamples() {
		if !matchesAnyTemplate(path, templates) {
			unmatched = append(unmatched, name+" -> "+path)
		}
	}
	sort.Strings(unmatched)
	for _, u := range unmatched {
		t.Errorf("apipath.%s matches no registered route. Either the route was "+
			"renamed and apipath was not updated, or apipath has a typo. Both "+
			"break the CLI at runtime.", u)
	}
}

// matchesAnyTemplate reports whether concrete matches a gin route
// template, where :param consumes one segment and *catchall the rest.
func matchesAnyTemplate(concrete string, templates []string) bool {
	for _, tmpl := range templates {
		if templateMatches(tmpl, concrete) {
			return true
		}
	}
	return false
}

func templateMatches(tmpl, concrete string) bool {
	tp := strings.Split(strings.Trim(tmpl, "/"), "/")
	cp := strings.Split(strings.Trim(concrete, "/"), "/")
	for i, seg := range tp {
		if strings.HasPrefix(seg, "*") {
			return true // catch-all swallows whatever remains, including nothing
		}
		if i >= len(cp) {
			return false
		}
		if strings.HasPrefix(seg, ":") {
			continue
		}
		if seg != cp[i] {
			return false
		}
	}
	return len(tp) == len(cp)
}

// The sample list must cover every exported path in apipath, or a new
// path could be added and silently never checked against the engine.
func TestAPIPathSamples_CoverEveryExportedPath(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join("..", "..", "pkg", "apipath", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob apipath: %v (found %d files)", err, len(files))
	}

	samples := apipathSamples()
	var missing []string

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, file, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", file, perr)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil || !d.Name.IsExported() {
					continue
				}
				if _, ok := samples[d.Name.Name]; !ok {
					missing = append(missing, "func "+d.Name.Name)
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, n := range vs.Names {
						// Base is the prefix every other path builds on,
						// not a route in its own right.
						if !n.IsExported() || n.Name == "Base" {
							continue
						}
						if _, ok := samples[n.Name]; !ok {
							missing = append(missing, "const "+n.Name)
						}
					}
				}
			}
		}
	}

	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("apipath.%s has no entry in apipathSamples, so it is never "+
			"checked against the engine. Add it.", m)
	}
}
