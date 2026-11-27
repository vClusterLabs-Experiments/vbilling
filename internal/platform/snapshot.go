package platform

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// Snapshot is one consistent read of the Platform objects that decide
// billing identity. A refresh either produces a whole snapshot or none.
type Snapshot struct {
	At        time.Time
	Self      Self
	Projects  map[string]*Object // by name
	Tenants   map[string]*Object // by name; nil when this Platform has no Tenant resource
	Instances []Object
	nsProject map[string]string // project namespace -> project
	// Watchable are the resources API discovery lists the watch verb for,
	// and Versions the resource version each was listed at.
	Watchable map[string]bool
	Versions  map[string]string
}

// Snapshot reads Self, Projects, Tenants (when the Platform has them) and
// VirtualClusterInstances. Any failure fails the whole read.
func (c *Client) Snapshot(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{At: now, Projects: map[string]*Object{}, nsProject: map[string]string{},
		Watchable: map[string]bool{}, Versions: map[string]string{}}
	self, err := c.Self(ctx)
	if err != nil {
		return nil, fmt.Errorf("read self: %w", err)
	}
	s.Self = self
	projects, rv, err := c.listAt(ctx, projectsGVR)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	s.Versions[projectsGVR.Resource] = rv
	for i := range projects {
		p := &projects[i]
		s.Projects[p.Name] = p
		if ns := p.Annotations[ProjectNamespaceAnnotation]; ns != "" {
			s.nsProject[ns] = p.Name
			continue
		}
		prefixes := []string{self.NamespacePrefix}
		if self.NamespacePrefix == "" {
			prefixes = []string{defaultNamespacePrefix, legacyNamespacePrefix}
		}
		for _, pre := range prefixes {
			s.nsProject[pre+p.Name] = p.Name
		}
	}
	verbs, err := c.resources()
	if err != nil {
		return nil, fmt.Errorf("discover management API: %w", err)
	}
	for res, vs := range verbs {
		s.Watchable[res] = slices.Contains(vs, "watch")
	}
	if _, hasTenants := verbs[tenantsGVR.Resource]; hasTenants {
		tenants, rv, err := c.listAt(ctx, tenantsGVR)
		if err != nil {
			return nil, fmt.Errorf("list tenants: %w", err)
		}
		s.Versions[tenantsGVR.Resource] = rv
		s.Tenants = map[string]*Object{}
		for i := range tenants {
			s.Tenants[tenants[i].Name] = &tenants[i]
		}
	}
	if s.Instances, rv, err = c.listAt(ctx, instancesGVR); err != nil {
		return nil, fmt.Errorf("list virtual cluster instances: %w", err)
	}
	s.Versions[instancesGVR.Resource] = rv
	return s, nil
}

// object returns the snapshot's version of a watched object: a Project or
// Tenant by name, an instance by UID. Nil when the snapshot does not have
// it.
func (s *Snapshot) object(resource string, o *Object) *Object {
	switch resource {
	case projectsGVR.Resource:
		return s.Projects[o.Name]
	case tenantsGVR.Resource:
		return s.Tenants[o.Name]
	}
	for i := range s.Instances {
		if s.Instances[i].UID == o.UID {
			return &s.Instances[i]
		}
	}
	return nil
}

// ownership is what of a Project, Tenant or instance decides who pays and
// under which name; nothing else (status, phase, other annotations) moves
// a cluster to another owner.
func ownership(o *Object) string {
	var b strings.Builder
	b.WriteString(string(o.UID) + "\x00" + o.Namespace + "/" + o.Name + "\x00" + o.Spec.DisplayName)
	ref := o.Spec.ClusterRef
	fmt.Fprintf(&b, "\x00%s/%s/%s/%t/%t", ref.Cluster, ref.Namespace, ref.VirtualCluster, o.Spec.Standalone, o.Status.IgnoreReconciliation)
	var keys []string
	for _, m := range []map[string]string{o.Labels, o.Annotations} {
		for k, v := range m {
			switch {
			case k == TenantOwnerLabel, k == TenantExclusiveLabel, k == TenantLegacyLabel, k == ProjectNamespaceAnnotation,
				strings.HasPrefix(k, discovery.AnnotationPrefix):
				keys = append(keys, k+"="+v)
			}
		}
	}
	sort.Strings(keys)
	b.WriteString("\x00" + strings.Join(keys, "\x00"))
	return b.String()
}

// HasTenants reports whether this Platform version has the Tenant resource.
func (s *Snapshot) HasTenants() bool { return s.Tenants != nil }

// index maps workloads (namespace/name on the control plane cluster) to the
// instances whose spec.clusterRef points at them. With cluster set, only
// instances on that connected cluster count.
func (s *Snapshot) index(cluster string) map[string][]*Object {
	out := map[string][]*Object{}
	for i := range s.Instances {
		inst := &s.Instances[i]
		ref := inst.Spec.ClusterRef
		if inst.Spec.Standalone || ref.Namespace == "" {
			continue // no workload on a control plane cluster
		}
		if cluster != "" && ref.Cluster != cluster {
			continue
		}
		name := ref.VirtualCluster
		if name == "" {
			name = inst.Name
		}
		key := ref.Namespace + "/" + name
		out[key] = append(out[key], inst)
	}
	for _, l := range out {
		sort.Slice(l, func(i, j int) bool { return l[i].Namespace+"/"+l[i].Name < l[j].Namespace+"/"+l[j].Name })
	}
	return out
}

// resolve binds one workload. A non-empty problem with ok=true is a warning
// (the binding stands); with ok=false nothing could be bound. prevUID is the
// instance the workload was bound to before, kept when the choice is
// ambiguous.
func (s *Snapshot) resolve(cands []*Object, prevUID string) (b discovery.PlatformBinding, problem string, ok bool) {
	if len(cands) > 1 {
		// During a move between projects the Platform keeps the old
		// instance with status.ignoreReconciliation until it is deleted.
		var live []*Object
		for _, c := range cands {
			if !c.Status.IgnoreReconciliation {
				live = append(live, c)
			}
		}
		if len(live) > 0 {
			cands = live
		}
	}
	if len(cands) == 0 {
		return b, "", false
	}
	inst := cands[0]
	if len(cands) > 1 {
		names := make([]string, len(cands))
		chosen := -1
		for i, c := range cands {
			names[i] = c.Namespace + "/" + c.Name
			if prevUID != "" && string(c.UID) == prevUID {
				chosen = i
			}
		}
		if chosen < 0 {
			return b, fmt.Sprintf("%d VirtualClusterInstances point at this cluster (%s); set PLATFORM_CLUSTER or remove the extra instance", len(cands), strings.Join(names, ", ")), false
		}
		inst = cands[chosen]
		problem = fmt.Sprintf("%d VirtualClusterInstances point at this cluster (%s); keeping %s", len(cands), strings.Join(names, ", "), names[chosen])
	}
	pname, found := s.nsProject[inst.Namespace]
	if !found {
		return b, fmt.Sprintf("instance %s/%s is in a namespace no readable Project owns", inst.Namespace, inst.Name), false
	}
	p := s.Projects[pname]
	b = discovery.PlatformBinding{
		Instance: inst.Name, InstanceUID: string(inst.UID), InstanceDisplayName: inst.Spec.DisplayName,
		InstanceCreated: inst.CreationTimestamp.UTC(), InstancePhase: inst.Status.Phase,
		Project: p.Name, ProjectUID: string(p.UID), ProjectDisplayName: p.Spec.DisplayName,
	}
	account := p
	if s.HasTenants() {
		name, tproblem := tenantOf(p)
		if tproblem != "" {
			problem = join(problem, tproblem)
		}
		if name != "" {
			b.Tenant = name
			if t, ok := s.Tenants[name]; ok {
				b.TenantUID, b.TenantDisplayName = string(t.UID), t.Spec.DisplayName
				account = t
			} else {
				problem = join(problem, fmt.Sprintf("Project %s names Tenant %s, which does not exist", p.Name, name))
			}
		}
	}
	b.Settings = settings(account)
	return b, problem, true
}

// tenantOf reads the Tenant a project belongs to from its labels. A project
// carrying both current labels is owned by no tenant (the Platform refuses
// that state and resolves it the same way).
func tenantOf(p *Object) (string, string) {
	owner, holder := p.Labels[TenantOwnerLabel], p.Labels[TenantExclusiveLabel]
	switch {
	case owner != "" && holder != "":
		return "", fmt.Sprintf("Project %s carries both %s and %s; billed as owned by no Tenant", p.Name, TenantOwnerLabel, TenantExclusiveLabel)
	case owner != "":
		return owner, ""
	case holder != "":
		return holder, ""
	}
	return p.Labels[TenantLegacyLabel], ""
}

// settings returns an object's vbilling.vcluster.com/ settings, annotations
// over labels.
func settings(o *Object) map[string]string {
	out := map[string]string{}
	for _, m := range []map[string]string{o.Labels, o.Annotations} {
		for k, v := range m {
			if s, ok := strings.CutPrefix(k, discovery.AnnotationPrefix); ok && v != "" {
				out[s] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
