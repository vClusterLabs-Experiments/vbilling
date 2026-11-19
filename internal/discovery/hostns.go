package discovery

import (
	"context"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

// With vcluster.yaml sync.toHost.namespaces, vCluster syncs a cluster's
// pods, volumes and services to namespaces of the control plane cluster of
// their own. The objects carry the cluster's name in
// vcluster.loft.sh/managed-by, which a cluster of the same name in another
// project sets too. vCluster's syncer labels every namespace it syncs into
// with the cluster's name and its own namespace (LabelSyncedName,
// LabelSyncedNamespace): those labels say whose the objects there are.
// Metering, Prometheus scoping, suspension and pause all decide with
// HostNamespaces.Whose.

// LabelSyncedName and LabelSyncedNamespace are the labels vCluster's syncer
// puts on each control plane cluster namespace it syncs a cluster's objects
// into: the cluster's name and the namespace the cluster runs in. They are
// set on the control plane cluster, after any extra labels the cluster's
// settings ask for, so a tenant can set neither.
const (
	LabelSyncedName      = "vcluster.loft.sh/vcluster-name"
	LabelSyncedNamespace = "vcluster.loft.sh/vcluster-namespace"
)

// labelManagedBy is the label vCluster's syncer puts on every object it
// syncs to the control plane cluster.
const labelManagedBy = "vcluster.loft.sh/managed-by"

// Ownership is whether an object a cluster's name marks as synced (it
// carries the name in vcluster.loft.sh/managed-by) is that cluster's.
type Ownership int

const (
	// NotOwned: another cluster's object.
	NotOwned Ownership = iota
	// Owned: the cluster's object.
	Owned
	// Uncertain: it may be the cluster's, and nothing tells: billed to no
	// one, and reported (Whose gives the reason).
	Uncertain
)

// Reasons Whose gives for Uncertain.
const (
	ReasonNameShared   = "namespace syncing with a cluster name used twice"
	ReasonOtherCluster = "synced into the namespace another cluster runs in"
	ReasonMissing      = "the namespace does not exist"
	ReasonUnread       = "the namespace could not be read"
)

// HostNamespaces is what vBilling read of the control plane cluster
// namespaces that hold synced objects: their vCluster labels, and which of
// them a vCluster control plane runs in. The zero value knows nothing.
type HostNamespaces struct {
	read   map[string]hostNamespace
	planes map[string]bool
	// clusters reports namespaces that discovered clusters run in; former,
	// namespaces clusters ran in before (UseClusters).
	clusters, former func(namespace string) bool
	// err is why the namespaces could not be read (Whose: ReasonUnread).
	err error
}

type hostNamespace struct {
	exists bool
	// name and namespace are vCluster's labels: both set, or both empty
	// (one without the other is no label at all).
	name, namespace string
}

// hostNamespaceGets bounds how many namespaces ReadHostNamespaces reads one
// by one; with more it lists them all.
const hostNamespaceGets = 32

// ReadHostNamespaces reads the namespaces given (those holding synced
// objects outside their cluster's namespace) and the namespaces vCluster
// control planes run in. It reads nothing without namespaces. A namespace
// that does not exist is noted as such. An error means the namespaces
// could not be read; a forbidden read is kept in the result instead
// (Whose then reports ReasonUnread), so a caller can go on.
func ReadHostNamespaces(ctx context.Context, kube kubernetes.Interface, namespaces []string) (*HostNamespaces, error) {
	h := &HostNamespaces{read: map[string]hostNamespace{}, planes: map[string]bool{}}
	if len(namespaces) == 0 {
		return h, nil
	}
	err := h.readNamespaces(ctx, kube, namespaces)
	if err == nil {
		err = h.readPlanes(ctx, kube)
	}
	if err != nil {
		if apierrors.IsForbidden(err) {
			h.read, h.planes, h.err = map[string]hostNamespace{}, map[string]bool{}, err
			return h, nil
		}
		return nil, err
	}
	return h, nil
}

func (h *HostNamespaces) readNamespaces(ctx context.Context, kube kubernetes.Interface, namespaces []string) error {
	if len(namespaces) <= hostNamespaceGets {
		for _, name := range namespaces {
			ns, err := kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
				h.read[name] = hostNamespace{}
			case err != nil:
				return fmt.Errorf("get namespace %s: %w", name, err)
			default:
				h.Note(name, true, ns.Labels)
			}
		}
		return nil
	}
	list, err := ListPaged(ctx, metav1.ListOptions{}, kube.CoreV1().Namespaces().List)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	for _, name := range namespaces {
		h.read[name] = hostNamespace{}
	}
	for i := range list.Items {
		if _, wanted := h.read[list.Items[i].Name]; wanted {
			h.Note(list.Items[i].Name, true, list.Items[i].Labels)
		}
	}
	return nil
}

// readPlanes notes the namespaces a vCluster control plane runs in, or ran
// in: vCluster's chart gives the cluster's Service the label app=vcluster,
// and it stays while the cluster's synced pods do (they are owned by it),
// also after its StatefulSet is deleted with --cascade=orphan for an
// upgrade, when discovery no longer lists the cluster. Services a cluster
// synced carry vcluster.loft.sh/managed-by and never count.
func (h *HostNamespaces) readPlanes(ctx context.Context, kube kubernetes.Interface) error {
	svcs, err := ListPaged(ctx, metav1.ListOptions{LabelSelector: "app=vcluster"}, kube.CoreV1().Services("").List)
	if err != nil {
		return fmt.Errorf("list vCluster services: %w", err)
	}
	for i := range svcs.Items {
		if svcs.Items[i].Labels[labelManagedBy] == "" {
			h.planes[svcs.Items[i].Namespace] = true
		}
	}
	return nil
}

// Note records what is known of a namespace: whether it exists, and its
// labels (only vCluster's two count). A namespace vBilling no longer finds
// can be noted as it was when it last did.
func (h *HostNamespaces) Note(namespace string, exists bool, labels map[string]string) {
	if h.read == nil {
		h.read = map[string]hostNamespace{}
	}
	n := hostNamespace{exists: exists}
	if name, ns := labels[LabelSyncedName], labels[LabelSyncedNamespace]; name != "" && ns != "" {
		n.name, n.namespace = name, ns
	}
	h.read[namespace] = n
}

// NoteControlPlane records that a vCluster control plane runs, or ran, in a
// namespace.
func (h *HostNamespaces) NoteControlPlane(namespace string) {
	if h.planes == nil {
		h.planes = map[string]bool{}
	}
	h.planes[namespace] = true
}

// UseClusters tells which namespaces discovered clusters run in, and which
// ones clusters ran in before (deleted since, or no longer discovered):
// what carries a cluster's name in a namespace without vCluster's labels
// where a cluster ran is that cluster's leftover. Either may be nil.
func (h *HostNamespaces) UseClusters(current, former func(namespace string) bool) {
	h.clusters, h.former = current, former
}

// Lookup returns what is known of a namespace: whether it exists and
// vCluster's labels on it (nil when it has none). known is false for a
// namespace that was not read.
func (h *HostNamespaces) Lookup(namespace string) (exists bool, labels map[string]string, known bool) {
	if h == nil {
		return false, nil, false
	}
	n, ok := h.read[namespace]
	if !ok {
		return false, nil, false
	}
	if n.name != "" {
		labels = map[string]string{LabelSyncedName: n.name, LabelSyncedNamespace: n.namespace}
	}
	return n.exists, labels, true
}

// ControlPlanes returns the namespaces a vCluster control plane runs in.
func (h *HostNamespaces) ControlPlanes() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.planes))
	for ns := range h.planes {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Err is why the namespaces could not be read (nil when they were).
func (h *HostNamespaces) Err() error {
	if h == nil {
		return nil
	}
	return h.err
}

// Whose decides whether an object in namespace that carries cl's name in
// vcluster.loft.sh/managed-by is cl's:
//   - in cl's own namespace it is;
//   - in a namespace another cluster runs in (discovered, a namesake, or
//     any vCluster control plane) it never is: the object is that
//     cluster's, or was synced where that cluster is billed;
//   - in a namespace vCluster labeled, the labels decide: they must name
//     cl and its namespace;
//   - in a namespace without vCluster's labels only the object's own label
//     names the cluster: it is cl's when no other vCluster has the name,
//     no cluster ran there, and the namespace exists; otherwise it is a
//     former cluster's leftover, or nothing tells (Uncertain).
//
// A cluster of the same name, deleted or not, never gets another's objects:
// its leftovers sit in the namespace it ran in, which its control plane's
// Service, discovery or vBilling's memory of it names.
func (h *HostNamespaces) Whose(cl TenantCluster, namespace string) (Ownership, string) {
	if cl.External {
		return NotOwned, ""
	}
	if namespace == cl.Namespace {
		return Owned, ""
	}
	var n hostNamespace
	read := false
	if h != nil {
		n, read = h.read[namespace]
	}
	mine := n.name == cl.Name && n.namespace == cl.Namespace
	if h != nil && (h.planes[namespace] || h.clusters != nil && h.clusters(namespace)) || slices.Contains(cl.Namesakes, namespace) {
		if mine {
			return Uncertain, ReasonOtherCluster
		}
		return NotOwned, ""
	}
	if n.name != "" {
		if mine {
			return Owned, ""
		}
		return NotOwned, ""
	}
	if h != nil && h.former != nil && h.former(namespace) {
		return NotOwned, ""
	}
	switch {
	case cl.NameShared():
		return Uncertain, ReasonNameShared
	case !read:
		return Uncertain, ReasonUnread
	case !n.exists:
		return Uncertain, ReasonMissing
	}
	return Owned, ""
}

// listPageSize is how many objects one list request asks for: every pod
// vCluster synced on a large control plane cluster is read a page at a time.
const listPageSize = 500

// ListPaged lists with list a page at a time (Limit and Continue) and
// returns every item in the first page's list. If the server expires the
// continue token midway, it lists once more in full.
func ListPaged[L runtime.Object](ctx context.Context, opts metav1.ListOptions, list func(context.Context, metav1.ListOptions) (L, error)) (L, error) {
	var zero L
	page := opts
	page.Limit = listPageSize
	first, err := list(ctx, page)
	if err != nil {
		return zero, err
	}
	m, err := meta.ListAccessor(first)
	if err != nil {
		return zero, err
	}
	if m.GetContinue() == "" {
		return first, nil
	}
	items, err := meta.ExtractList(first)
	if err != nil {
		return zero, err
	}
	// Later pages are read at the first one's resource version, which the
	// continue token carries.
	page.ResourceVersion, page.ResourceVersionMatch = "", ""
	for cont := m.GetContinue(); cont != ""; {
		page.Continue = cont
		next, err := list(ctx, page)
		if apierrors.IsResourceExpired(err) {
			full := opts
			full.Limit, full.Continue = 0, ""
			return list(ctx, full)
		}
		if err != nil {
			return zero, err
		}
		more, err := meta.ExtractList(next)
		if err != nil {
			return zero, err
		}
		items = append(items, more...)
		nm, err := meta.ListAccessor(next)
		if err != nil {
			return zero, err
		}
		cont = nm.GetContinue()
	}
	if err := meta.SetList(first, items); err != nil {
		return zero, err
	}
	m.SetContinue("")
	return first, nil
}

// SyncedPodsOf lists the pods cl synced to the control plane cluster: in
// its namespace, and in other namespaces where Whose says they are its.
// fieldSelector narrows the pods (for example to one node); clusters
// reports namespaces discovered clusters run in (nil: none besides cl's
// namesakes and vCluster control planes).
func SyncedPodsOf(ctx context.Context, kube kubernetes.Interface, cl TenantCluster, fieldSelector string, clusters func(string) bool) ([]corev1.Pod, error) {
	pods, err := ListPaged(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + cl.Name, FieldSelector: fieldSelector},
		kube.CoreV1().Pods("").List)
	if err != nil {
		return nil, fmt.Errorf("list synced pods: %w", err)
	}
	var other []string
	seen := map[string]bool{cl.Namespace: true}
	for i := range pods.Items {
		if ns := pods.Items[i].Namespace; !seen[ns] {
			seen[ns] = true
			other = append(other, ns)
		}
	}
	host, err := ReadHostNamespaces(ctx, kube, other)
	if err != nil {
		return nil, err
	}
	host.UseClusters(clusters, nil)
	var out []corev1.Pod
	for _, p := range pods.Items {
		if p.Labels[labelManagedBy] != cl.Name {
			continue
		}
		if own, _ := host.Whose(cl, p.Namespace); own == Owned {
			out = append(out, p)
		}
	}
	return out, nil
}
