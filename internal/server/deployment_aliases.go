package server

// Re-exports of deployment domain types from pkg/modelregistry.
// The canonical definitions live in pkg/modelregistry/deployment_tracker.go;
// this file preserves the unqualified names used throughout the server layer
// so the migration is a single-file change rather than a 30-file rename.

import (
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

type Deployment = modelregistry.Deployment
type DeploymentNode = modelregistry.DeploymentNode
type DeploymentTracker = modelregistry.DeploymentTracker

var NewDeploymentTracker = modelregistry.NewDeploymentTracker
