package prov_apps

import (
	"context"
	"fmt"
	"os"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// DeleteDisposable removes only a managed plan destination after runs have stopped.
func (c *InstallCoordinator) DeleteDisposable(ctx context.Context, provider, runtime, identity string) error {
	if c.shutdown.Load() {
		return ErrShutdown
	}
	if !install.ValidPlanID(identity) {
		return fmt.Errorf("invalid disposable_plan_id")
	}
	if runtime == "" {
		runtime = provider
	}
	if c.appsConfig == nil || c.appsConfig() == nil {
		return ErrProviderNotFound
	}
	sc, ok := c.appsConfig().LookupApp(provider)
	if !ok || sc.Install == nil {
		return ErrProviderNotFound
	}
	if _, ok := sc.Install.Runtimes[runtime]; !ok {
		return fmt.Errorf("runtime not declared by provider")
	}
	unlock, err := c.acquireProvider(ctx, runtime)
	if err != nil {
		return err
	}
	defer unlock()
	if c.instances.CountActiveByProvider(provider) > 0 {
		return ErrInstancesRunning
	}
	lock := fsroot.NewLockFile(runtime)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	path := install.DisposableDir(runtime, identity)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("disposable destination is not a managed directory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
