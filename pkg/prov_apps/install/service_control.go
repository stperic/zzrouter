package install

import (
	"context"
	"fmt"
)

// ServiceAction is a fixed lifecycle operation executed by the runtime owner.
type ServiceAction string

const (
	ServiceStart   ServiceAction = "start"
	ServiceStop    ServiceAction = "stop"
	ServiceRestart ServiceAction = "restart"
)

type serviceControlKey struct{}

// WithServiceControl binds an install operation to its owning provider lifecycle.
func WithServiceControl(ctx context.Context, control func(context.Context, ServiceAction) error) context.Context {
	return context.WithValue(ctx, serviceControlKey{}, control)
}

func executeServiceAction(ctx context.Context, action ServiceAction) error {
	if action != ServiceStart && action != ServiceStop && action != ServiceRestart {
		return fmt.Errorf("unsupported provider service action %q", action)
	}
	control, ok := ctx.Value(serviceControlKey{}).(func(context.Context, ServiceAction) error)
	if !ok {
		return fmt.Errorf("managed service steps must run through zzRouter's provider installation API")
	}
	return control(ctx, action)
}
