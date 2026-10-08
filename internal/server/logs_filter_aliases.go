package server

// Re-exports of log filter types from pkg/inferencelog.
// The canonical definitions live in pkg/inferencelog/filter.go;
// this file preserves the names used throughout the server layer.

import (
	"github.com/stperic/zzrouter/pkg/inferencelog"
)

type logFilter = inferencelog.LogFilter

var compileLogFilter = inferencelog.CompileLogFilter

const (
	logFilterMaxContext         = inferencelog.LogFilterMaxContext
	logFilterDefaultMaxMatches  = inferencelog.LogFilterDefaultMaxMatches
	logFilterAbsoluteMaxMatches = inferencelog.LogFilterAbsoluteMaxMatches
)
