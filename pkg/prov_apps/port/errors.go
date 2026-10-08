package port

import "errors"

// ErrPortConflict is returned by AppPoolManager.validateNoOverlaps when
// two configured port ranges for different providers overlap. Callers
// discriminate with errors.Is; wrapped raise sites add the overlapping
// range pair as context.
//
// Re-exported from pkg/prov_apps so the root-level error surface stays
// stable for existing consumers.
var ErrPortConflict = errors.New("port range conflict")
