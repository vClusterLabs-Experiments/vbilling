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

// AccountSettings are the vbilling.vcluster.com/ settings that decide who is
// billed and at what price. For a cluster bound to vCluster Platform they
// come only from the Tenant or Project: workload, namespace and
// VirtualClusterInstance metadata can be set by the people who create
// clusters (vcluster.yaml sets workload labels), so it never decides them.
var AccountSettings = map[string]bool{
	"tenant": true, "project": true, "display-name": true, "email": true, "currency": true,
	"plan": true, "tenant-class": true, "cost-center": true, "aliases": true, "exclude": true,
	"stripe-customer-id": true, "metronome-customer-id": true,
}

// ApplyPlatform binds c to b: tenant, project, instance and account
// settings come from the Platform objects, and account settings found on
// the workload or namespace are dropped. Other settings (for example
// prometheus-url) stay per cluster; the Tenant or Project only supplies
// defaults for them.
func (c *TenantCluster) ApplyPlatform(b PlatformBinding) {
	c.Platform = &b
	c.Project = b.Project
	c.Instance = b.Instance
	if b.InstanceDisplayName != "" {
		c.DisplayName = b.InstanceDisplayName
	}
	c.Labels = dropAccountSettings(c.Labels)
	c.Annotations = dropAccountSettings(c.Annotations)
	for k, v := range b.Settings {
		if k == "project" {
			continue // the Platform project is the project
		}
		key := AnnotationPrefix + k
		if AccountSettings[k] || (c.Annotations[key] == "" && c.Labels[key] == "") {
			c.Annotations[key] = v
		}
	}
	c.Annotations[AnnotationPrefix+"display-name"] = b.AccountDisplayName()
}

func dropAccountSettings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := strings.CutPrefix(k, AnnotationPrefix); ok && AccountSettings[s] {
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
	if b.Tenant != "" {
		return map[string]string{"platform_account": "tenant", "platform_tenant": b.Tenant, "platform_tenant_uid": b.TenantUID}
	}
	return map[string]string{"platform_account": "project", "platform_project": b.Project, "platform_project_uid": b.ProjectUID}
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
