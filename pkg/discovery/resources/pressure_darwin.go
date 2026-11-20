package resources

import "golang.org/x/sys/unix"

func pressureLevel() *int {
	value, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return nil
	}
	return pressure(value)
}
