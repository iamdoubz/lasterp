// SPDX-License-Identifier: AGPL-3.0-only

package capability

import (
	"context"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// GatewayChecker adapts the registry + enable-state to the API gateway's
// capability seam (it satisfies kernel/api.CapabilityChecker structurally, so
// neither package imports the other). Objects owned by no module — kernel
// objects — are always enabled.
type GatewayChecker struct {
	Reg *Registry
	DB  *storage.DB
}

// Enabled reports whether the module owning object is enabled for tenant, and
// the capability name to surface if not.
func (c GatewayChecker) Enabled(ctx context.Context, tenant tenancy.ID, object string) (bool, string, error) {
	module, owned := c.Reg.ModuleForObject(object)
	if !owned {
		return true, "", nil // kernel object: always reachable
	}
	m := c.Reg.modules[module]
	if m != nil && m.Kernel {
		return true, "", nil
	}
	on, err := IsModuleEnabled(ctx, c.DB, tenant, module)
	if err != nil {
		return false, "", err
	}
	capName := ""
	if m != nil {
		capName = m.primaryCapability()
	}
	return on, capName, nil
}
