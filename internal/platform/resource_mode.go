package platform

type ResourceMode string

const (
	ResourceModeShared          ResourceMode = "shared"
	ResourceModeDedicatedTenant ResourceMode = "dedicated-tenant"
	ResourceModeDedicatedRule   ResourceMode = "dedicated-rule"
)

func normalizeResourceMode(value string) (ResourceMode, error) {
	mode := ResourceMode(value)
	switch mode {
	case ResourceModeShared, ResourceModeDedicatedTenant, ResourceModeDedicatedRule:
		return mode, nil
	default:
		return "", ErrInvalid
	}
}
