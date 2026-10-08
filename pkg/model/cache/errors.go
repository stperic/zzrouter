package cache

import "fmt"

// ErrModelNotFound indicates the model was not found in the cache
// (or, on a worker, in the local registry).
type ErrModelNotFound struct {
	ModelName string
}

func (e ErrModelNotFound) Error() string {
	return fmt.Sprintf("model '%s' not found", e.ModelName)
}
