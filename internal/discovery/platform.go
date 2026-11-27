package discovery

import (
	"strings"
	"time"
)

// PlatformBinding is a cluster's owner in vCluster Platform: the
// VirtualClusterInstance whose spec.clusterRef points at the cluster's
// workload, the Project that holds it and, when the Platform has one, the
// Tenant that owns the project. Names are what invoices show; UIDs stay
// stable when a name is reused after a deletion.
type PlatformBinding struct {
	Instance            string    `json:"instance"`
	InstanceUID         string    `json:"instanceUID,omitempty"`
	InstanceDisplayName string    `json:"instanceDisplayName,omitempty"`
	InstanceCreated     time.Time `json:"instanceCreated,omitempty"`
	// InstancePhase is the instance's status.phase (Ready, Sleeping, ...).
	InstancePhase      string `json:"instancePhase,omitempty"`
	Project            string `json:"project"`
	ProjectUID         string `json:"projectUID,omitempty"`
	ProjectDisplayName string `json:"projectDisplayName,omitempty"`
	// Tenant is the Platform Tenant that owns the project; empty on
	// Platform versions without Tenants and for projects no Tenant owns.
	Tenant            string `json:"tenant,omitempty"`
	TenantUID         string `json:"tenantUID,omitempty"`
	TenantDisplayName string `json:"tenantDisplayName,omitempty"`
	// Settings are the vbilling.vcluster.com/ settings (prefix trimmed) of
	// the billing account object: the Tenant, else the Project.
	Settings map[string]string `json:"settings,omitempty"`
	// PreviousTenantID is the billing ID this cluster's account had before
	// its tenant setting was changed (at TenantIDChanged). The account is
	// the same Tenant or Project; the billing platform sees another
	// customer.
	PreviousTenantID string    `json:"previousTenantID,omitempty"`
	TenantIDChanged  time.Time `json:"tenantIDChanged,omitempty"`
}

// SameAccount reports whether two bindings have the same billing account
// object: the same Tenant, or (without Tenants) the same Project.
func (b *PlatformBinding) SameAccount(o *PlatformBinding) bool {
	if b == nil || o == nil {
		return false
	}
	if b.Tenant != "" || o.Tenant != "" {
		return b.TenantUID != "" && b.TenantUID == o.TenantUID
	}
	return b.ProjectUID != "" && b.ProjectUID == o.ProjectUID
}

// TenantID is the billing customer: an explicit vbilling.vcluster.com/tenant
// on the Tenant or Project, else tenant-<Tenant>, else project-<Project>
// (the same ID TENANT_SOURCE=project uses).
func (b *PlatformBinding) TenantID() string {
	if v := b.Settings["tenant"]; v != "" {
		return v
	}
	if b.Tenant != "" {
		return "tenant-" + b.Tenant
	}
	return "project-" + b.Project
}

// AccountDisplayName is the billing customer's name: an explicit
// display-name setting, else the Tenant's (or Project's) display name, else
// its name.
func (b *PlatformBinding) AccountDisplayName() string {
	if v := b.Settings["display-name"]; v != "" {
		return v
	}
	if b.Tenant != "" {
		return firstNonEmpty(b.TenantDisplayName, b.Tenant)
	}
	return firstNonEmpty(b.ProjectDisplayName, b.Project)
}

// Owner is who decides the billing tenant, for logs and the status API.
func (b *PlatformBinding) Owner() string {
	if b.Tenant != "" {
		return "Tenant " + b.Tenant + " (Project " + b.Project + ")"
	}
	return "Project " + b.Project
}

// Same reports whether two bindings bill the same instance to the same owner.
func (b *PlatformBinding) Same(o *PlatformBinding) bool {
	if b == nil || o == nil {
		return b == o
	}
	return b.InstanceUID == o.InstanceUID && b.Project == o.Project && b.Tenant == o.Tenant && b.TenantID() == o.TenantID()
}

// stateAnnotations are the workload annotations vBilling writes itself to
// mark its own pauses. They are state, not settings, so binding a cluster
// to its Platform owner keeps them.
var stateAnnotations = map[string]bool{AnnotationPausedBy: true, AnnotationPausedAt: true, AnnotationPausedReplicas: true}

// ApplyPlatform binds c to b. Tenant, project, instance and every
// vbilling.vcluster.com/ setting come from the Platform objects: the
// settings of the Tenant, else the Project, plus the operator's own
// configuration. Settings found on the cluster's workload, namespace or
// VirtualClusterInstance are dropped, whatever they are: the people who
// create clusters write that metadata (vcluster.yaml sets workload labels
// and annotations), and they must never choose who pays, at what price, or
// how their usage is read (prometheus-url, prometheus-matchers,
// prometheus-source).
func (c *TenantCluster) ApplyPlatform(b PlatformBinding) {
	c.Platform = &b
	c.Project = b.Project
	c.Instance = b.Instance
	if b.InstanceDisplayName != "" {
		c.DisplayName = b.InstanceDisplayName
	}
	c.Labels = dropSettings(c.Labels)
	c.Annotations = dropSettings(c.Annotations)
	for k, v := range b.Settings {
		if k == "project" {
			continue // the Platform project is the project
		}
		c.Annotations[AnnotationPrefix+k] = v
	}
	c.Annotations[AnnotationPrefix+"display-name"] = b.AccountDisplayName()
}

// dropSettings returns m without vbilling.vcluster.com/ settings, keeping
// vBilling's own state annotations.
func dropSettings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.HasPrefix(k, AnnotationPrefix) && !stateAnnotations[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// PlatformMetadata is what a billing customer record carries about its
// Platform owner, for traceability in the billing platform.
func (c *TenantCluster) PlatformMetadata() map[string]string {
	b := c.Platform
	if b == nil {
		return nil
	}
	meta := map[string]string{"platform_account": "project", "platform_project": b.Project, "platform_project_uid": b.ProjectUID}
	if b.Tenant != "" {
		meta = map[string]string{"platform_account": "tenant", "platform_tenant": b.Tenant, "platform_tenant_uid": b.TenantUID}
	}
	if b.PreviousTenantID != "" {
		// The same account billed under another ID before: whoever reads the
		// customer record can find its earlier usage and its wallet.
		meta["previous_tenant_id"] = b.PreviousTenantID
		meta["tenant_id_changed_at"] = b.TenantIDChanged.UTC().Format(time.RFC3339)
	}
	return meta
}

// PlatformProperties are stamped on the cluster's usage events: the
// instance, project and tenant UIDs identify the owner even after a name is
// reused, and outlive moves between projects.
func (c *TenantCluster) PlatformProperties() map[string]any {
	b := c.Platform
	if b == nil {
		return nil
	}
	p := map[string]any{"platform_instance": b.Instance}
	for k, v := range map[string]string{"platform_instance_uid": b.InstanceUID, "platform_project_uid": b.ProjectUID, "platform_tenant_uid": b.TenantUID} {
		if v != "" {
			p[k] = v
		}
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
