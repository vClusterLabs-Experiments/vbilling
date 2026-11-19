package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A cluster's own nodes (Private Nodes, Auto Nodes, Standalone) are read
// through the cluster's API, and everything about them there (labels,
// taints, status) is written from inside the cluster: by its kubelets and
// device plugins, and by its admins, who are cluster admins. None of it may
// lower what such a node bills. Exemptions belong to the operator, not to a
// node's labels; see hostViews for the nodes that are not the cluster's own,
// and privateNodeHistory for what a node bills as.

// hostViews decides which nodes a cluster's own API reports are nodes of
// the control plane cluster seen through it: those are metered there, as
// shared or dedicated nodes, never as the cluster's own. A node's own labels
// never decide (anyone who can write the node can label it). A node is the
// control plane cluster's when a control plane cluster node has its name and
//   - it reports the same machine (status.nodeInfo systemUUID or
//     machineID): the copy of a real node vCluster syncs into a cluster on
//     shared nodes; or
//   - pods this cluster synced to the control plane cluster run on that
//     node: vCluster's placeholder ("fake") node, which it creates only for
//     the nodes its pods run on, with machine IDs of its own.
//
// A cluster with nodes of its own syncs no pods, and it cannot see the
// control plane cluster's machine IDs, so neither rule exempts a node it
// runs.
type hostViews struct {
	c        *Collector
	cl       discovery.TenantCluster
	external bool
	host     map[string]*nodeInfo
	// syncedOn are the control plane cluster nodes the cluster's synced pods
	// run on (nil until needed: read then).
	syncedOn map[string]bool
	decided  map[string]bool
}

func (c *Collector) newHostViews(cl discovery.TenantCluster, external bool, host map[string]*nodeInfo, syncedOn map[string]bool) *hostViews {
	return &hostViews{c: c, cl: cl, external: external, host: host, syncedOn: syncedOn, decided: map[string]bool{}}
}

// view reports whether n is a control plane cluster node seen through the
// cluster. An error means it cannot tell; the cluster's read fails then,
// and its windows are filled once it can.
func (v *hostViews) view(ctx context.Context, n *corev1.Node) (bool, error) {
	if v.external {
		return false, nil // shares no nodes with this control plane cluster
	}
	h, ok := v.host[n.Name]
	if !ok {
		return false, nil
	}
	if sameMachine(h, n) {
		return true, nil
	}
	if is, ok := v.decided[n.Name]; ok {
		return is, nil
	}
	if v.syncedOn == nil {
		on, err := v.c.syncedPodNodes(ctx, v.cl)
		if err != nil {
			return false, err
		}
		v.syncedOn = on
	}
	is := v.syncedOn[n.Name]
	if !is {
		// A placeholder for a pod synced after the cluster's pods were
		// listed: vCluster creates it only once that pod is on the node.
		var err error
		if is, err = v.c.syncedPodOn(ctx, v.cl, n.Name); err != nil {
			return false, err
		}
	}
	v.decided[n.Name] = is
	return is, nil
}

// classify records in inv which of its nodes are views of control plane
// cluster nodes, so a paused cluster metered from it later decides the same.
func (v *hostViews) classify(ctx context.Context, inv *tenantInventory) error {
	inv.Views, inv.Classified = nil, true
	for i := range inv.Nodes {
		is, err := v.view(ctx, &inv.Nodes[i])
		if err != nil {
			return err
		}
		if is {
			inv.Views = append(inv.Views, inv.Nodes[i].Name)
		}
	}
	return nil
}

// storedViews decides like hostViews for an inventory saved earlier (a
// paused cluster): as classified when it was read. An inventory saved by an
// earlier vBilling was not classified; its nodes are decided as that
// version did.
func storedViews(inv *tenantInventory, host map[string]*nodeInfo, external bool) func(context.Context, *corev1.Node) (bool, error) {
	if inv.Classified {
		views := map[string]bool{}
		for _, name := range inv.Views {
			views[name] = true
		}
		return func(_ context.Context, n *corev1.Node) (bool, error) { return views[n.Name], nil }
	}
	return func(_ context.Context, n *corev1.Node) (bool, error) {
		_, onHost := host[n.Name]
		return onHost && !external || n.Labels[LabelFakeNode] == "true", nil
	}
}

// sameMachine reports whether a node read through a cluster is the control
// plane cluster node h: vCluster copies the status of the real nodes it
// syncs, machine IDs included.
func sameMachine(h *nodeInfo, n *corev1.Node) bool {
	uuid, machine := strings.TrimSpace(n.Status.NodeInfo.SystemUUID), strings.TrimSpace(n.Status.NodeInfo.MachineID)
	return h.systemUUID != "" && strings.EqualFold(h.systemUUID, uuid) || h.machineID != "" && h.machineID == machine
}

// syncedPodNodes returns the control plane cluster nodes that pods a
// cluster synced there run on, deleted ones included (vCluster keeps a
// placeholder until its last pod is gone).
func (c *Collector) syncedPodNodes(ctx context.Context, cl discovery.TenantCluster) (map[string]bool, error) {
	pods, err := c.syncedPodsOf(ctx, cl, "")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range pods {
		if p.Spec.NodeName != "" {
			out[p.Spec.NodeName] = true
		}
	}
	return out, nil
}

// syncedPodOn reports whether a pod the cluster synced runs on a control
// plane cluster node.
func (c *Collector) syncedPodOn(ctx context.Context, cl discovery.TenantCluster, node string) (bool, error) {
	pods, err := c.syncedPodsOf(ctx, cl, "spec.nodeName="+node)
	if err != nil {
		return false, fmt.Errorf("pods synced to node %s: %w", node, err)
	}
	for _, p := range pods {
		if p.Spec.NodeName == node {
			return true, nil
		}
	}
	return false, nil
}

// syncedNodes adds the nodes the given pods run on to on.
func syncedNodes(on map[string]bool, pods []podEntry, include func(*corev1.Pod) bool) {
	for _, e := range pods {
		if p := e.pod; p.Spec.NodeName != "" && include(p) {
			on[p.Spec.NodeName] = true
		}
	}
}

const (
	// gpuStableAfter: without GPU Feature Discovery's count, a node's first
	// GPU floor is a count two reads at least this far apart both saw.
	gpuStableAfter = 30 * time.Second
	// gpuRaiseAfter: without GPU Feature Discovery's count, a count above a
	// node's floor raises it once it held this long. When MIG or
	// time-slicing is turned on, the device plugin advertises the new
	// devices at once, while the labels that convert them back to physical
	// GPUs follow a minute or two later (Node Feature Discovery relabels
	// every 60 seconds by default).
	gpuRaiseAfter = 10 * time.Minute
	// attributeFillAfterCreation: a node's zone, capacity type, SKU and
	// instance type first seen empty are filled from labels read within
	// this long of its creation: the cloud controller manager and Karpenter
	// label a node seconds after its kubelet registers it.
	attributeFillAfterCreation = 10 * time.Minute
	// nodeHistoryRetention: a node not seen for this long is forgotten.
	nodeHistoryRetention = 60 * 24 * time.Hour
	// nodeHistorySeenEvery bounds how stale a saved last-seen time gets, so
	// the file is not rewritten every window for it.
	nodeHistorySeenEvery = time.Hour
)

// privateNodeHistory remembers, for each node of a cluster's own (by
// cluster and node name: the kubelet reads its node by name, so a cluster
// admin who deletes the node object and creates it again gets the same
// record), what vBilling first saw of it and the most GPUs it had, so that
// nothing written later inside the cluster lowers what it bills:
//   - Its GPU model, instance type, SKU (SKU_LABEL), capacity type and zone
//     stay as first seen. A GPU model first seen unknown (before GPU Feature
//     Discovery labeled the node) is taken from the first label that names
//     one; the others, first seen empty, from labels set within
//     attributeFillAfterCreation of the node's creation.
//   - It is billed for no fewer GPUs than it had while it lived (its GPU
//     floor). With GPU Feature Discovery's physical count
//     (nvidia.com/gpu.count), the floor rises to what the node advertises,
//     up to that count. Without it, two reads at least gpuStableAfter apart
//     set the first floor, and a higher count raises it only once it held
//     for gpuRaiseAfter: devices advertised before the labels that convert
//     them to physical GPUs (MIG, time-slicing) never stick.
//
// A change is logged and reported as diverged in source health. Kept in
// DATA_DIR/private-nodes.json (UsePrivateNodeHistory); nodes not seen for
// nodeHistoryRetention are forgotten.
type privateNodeHistory struct {
	mu     sync.Mutex
	path   string // "" keeps it in memory only
	nodes  map[string]*nodeRecord
	dirty  bool
	logged map[string]string // node key -> divergence last logged
	// diverged are the clusters whose private-nodes source reports nodes
	// that changed (the source is reported only while they do, and once
	// when they no longer do).
	diverged map[string]bool
}

type nodeRecord struct {
	Cluster      string  `json:"cluster"`
	Node         string  `json:"node"`
	GPUs         float64 `json:"gpus,omitempty"` // high-water mark of physical GPUs
	GPUType      string  `json:"gpu_type,omitempty"`
	InstanceType string  `json:"instance_type,omitempty"`
	SKU          string  `json:"sku,omitempty"`
	// CapacityType is empty for a node first seen without a capacity type
	// label: it bills as on-demand.
	CapacityType string    `json:"capacity_type,omitempty"`
	Zone         string    `json:"zone,omitempty"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	// Created is the creation time of the node object vBilling first saw
	// (zero in records of earlier versions: nothing is filled then).
	Created time.Time `json:"created,omitempty"`

	// The count of an earlier read and when, for the first floor; and since
	// when, and at least how many, GPUs above the floor were advertised.
	readGPUs   float64
	readAt     time.Time
	raiseGPUs  float64
	raiseSince time.Time
}

func newPrivateNodeHistory() *privateNodeHistory {
	return &privateNodeHistory{nodes: map[string]*nodeRecord{}, logged: map[string]string{}, diverged: map[string]bool{}}
}

// UsePrivateNodeHistory keeps what vBilling first saw of each node of a
// cluster's own, and the most GPUs it had, in the file at path (in
// DATA_DIR), so a restart does not forget them. Records of earlier versions,
// kept by node UID, are moved to their cluster and node name; two records
// of one node (its object was created again) become one, as first seen.
func (c *Collector) UsePrivateNodeHistory(path string) error {
	h := newPrivateNodeHistory()
	h.path = path
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
	case err != nil:
		return err
	case len(b) > 0:
		var stored map[string]*nodeRecord
		if err := json.Unmarshal(b, &stored); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		keys := make([]string, 0, len(stored))
		for k := range stored {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r := stored[k]
			if r == nil {
				continue
			}
			key := k
			if r.Cluster != "" && r.Node != "" {
				key = r.Cluster + "/" + r.Node
			}
			if prev := h.nodes[key]; prev != nil {
				r = mergeRecords(prev, r)
			}
			if key != k {
				h.dirty = true
			}
			h.nodes[key] = r
		}
	}
	c.nodeHistory = h
	return nil
}

// mergeRecords makes one record of two of the same node: what the earlier
// one first saw, and the most GPUs either had.
func mergeRecords(a, b *nodeRecord) *nodeRecord {
	first, other := a, b
	if b.FirstSeen.Before(a.FirstSeen) || b.FirstSeen.Equal(a.FirstSeen) && b.GPUs > a.GPUs {
		first, other = b, a
	}
	out := *first
	out.GPUs = max(a.GPUs, b.GPUs)
	if other.LastSeen.After(out.LastSeen) {
		out.LastSeen = other.LastSeen
	}
	if out.GPUType == "" {
		out.GPUType = other.GPUType
	}
	return &out
}

// apply sets a node's GPU floor and its first-seen attributes from its
// record, creating the record on first sight. It returns what changed since.
func (h *privateNodeHistory) apply(cluster string, n *nodeInfo, now time.Time) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.nodes[nodeKey(cluster, n)]
	if r == nil {
		r = &nodeRecord{Cluster: cluster, Node: n.name, InstanceType: n.instanceType, SKU: n.sku,
			Zone: n.zone, FirstSeen: now, LastSeen: now, Created: n.created}
		if n.capacityLabeled {
			r.CapacityType = n.capacityType
		}
		h.nodes[nodeKey(cluster, n)] = r
		h.dirty = true
	}
	if now.Sub(r.LastSeen) >= nodeHistorySeenEvery {
		r.LastSeen, h.dirty = now, true
	}
	if r.GPUType == "" && n.gpuType != "unknown" && n.gpuType != "" {
		r.GPUType, h.dirty = n.gpuType, true
	}
	if !r.Created.IsZero() && now.Before(r.Created.Add(attributeFillAfterCreation)) {
		fill := func(first *string, cur string) {
			if *first == "" && cur != "" {
				*first, h.dirty = cur, true
			}
		}
		fill(&r.InstanceType, n.instanceType)
		fill(&r.SKU, n.sku)
		fill(&r.Zone, n.zone)
		if n.capacityLabeled {
			fill(&r.CapacityType, n.capacityType)
		}
	}
	var notes []string
	keep := func(what, first string, cur *string) {
		if *cur != first {
			notes = append(notes, fmt.Sprintf("%s %s (first seen: %s)", what, orNone(*cur), orNone(first)))
			*cur = first
		}
	}
	if r.GPUType != "" {
		keep("GPU model", r.GPUType, &n.gpuType)
	}
	keep("instance type", r.InstanceType, &n.instanceType)
	keep("SKU", r.SKU, &n.sku)
	firstCapacity := r.CapacityType
	if firstCapacity == "" {
		firstCapacity = usage.CapacityOnDemand
	}
	keep("capacity type", firstCapacity, &n.capacityType)
	keep("zone", r.Zone, &n.zone)

	cur := n.advertisedGPUs()
	switch counted := n.expectedGPUs(); {
	case counted > 0:
		// GPU Feature Discovery counts the node's physical GPUs: the floor
		// rises to what the node advertises, as far as that count goes.
		if v := min(cur, counted); v > r.GPUs {
			r.GPUs, h.dirty = v, true
		}
		r.raiseSince = time.Time{}
	case r.GPUs == 0:
		// The first floor: a count two reads gpuStableAfter apart both saw.
		if r.readAt.IsZero() || now.Sub(r.readAt) >= gpuStableAfter {
			if !r.readAt.IsZero() {
				if stable := min(r.readGPUs, cur); stable > r.GPUs {
					r.GPUs, h.dirty = stable, true
				}
			}
			r.readGPUs, r.readAt = cur, now
		} else {
			r.readGPUs = min(r.readGPUs, cur) // read again moments later: the lower one counts
		}
	case cur > r.GPUs:
		// More than the floor, with nothing that counts physical GPUs: it
		// raises the floor once it held for gpuRaiseAfter.
		if r.raiseSince.IsZero() {
			r.raiseSince, r.raiseGPUs = now, cur
		} else {
			r.raiseGPUs = min(r.raiseGPUs, cur)
		}
		if now.Sub(r.raiseSince) >= gpuRaiseAfter {
			r.GPUs, h.dirty = r.raiseGPUs, true
			r.raiseSince = time.Time{}
		}
	default:
		r.raiseSince = time.Time{}
	}
	n.gpuFloor = r.GPUs
	if cur < r.GPUs {
		notes = append(notes, fmt.Sprintf("advertises %s GPUs, billed for %s, the most it had", fmtGPUs(cur), fmtGPUs(r.GPUs)))
	}
	return notes
}

// nodeKey is a node's record: by cluster and node name, never by the node
// object's UID, which a cluster admin renews by creating the object again.
func nodeKey(cluster string, n *nodeInfo) string {
	return cluster + "/" + n.name
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func fmtGPUs(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// noteChanges logs a node's divergence when it changes.
func (h *privateNodeHistory) noteChanges(cluster string, n *nodeInfo, notes []string) {
	key := nodeKey(cluster, n)
	msg := strings.Join(notes, "; ")
	h.mu.Lock()
	prev, had := h.logged[key]
	if msg == "" {
		delete(h.logged, key)
	} else {
		h.logged[key] = msg
	}
	h.mu.Unlock()
	switch {
	case msg != "" && msg != prev:
		log.Printf("[metrics] %s: node %s changed since vBilling first saw it: %s. It is billed as first seen", cluster, n.name, msg)
	case msg == "" && had:
		log.Printf("[metrics] %s: node %s is again as vBilling first saw it", cluster, n.name)
	}
}

// save writes the history if it changed, after forgetting nodes not seen
// for nodeHistoryRetention.
func (h *privateNodeHistory) save(now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, r := range h.nodes {
		if now.Sub(r.LastSeen) > nodeHistoryRetention {
			delete(h.nodes, k)
			delete(h.logged, k)
			h.dirty = true
		}
	}
	if !h.dirty || h.path == "" {
		h.dirty = false
		return nil
	}
	b, err := json.Marshal(h.nodes)
	if err != nil {
		return err
	}
	if err := writeFileSync(h.path, b); err != nil {
		return err
	}
	h.dirty = false
	return nil
}

// writeFileSync replaces path with b durably: a temporary file, fsynced and
// renamed over it, then the directory fsynced.
func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// maxDivergedNotes bounds the message of a cluster's private-nodes source.
const maxDivergedNotes = 5

// rememberPrivateNodes applies the history to a cluster's own nodes and
// reports the ones that changed since vBilling first saw them.
func (c *Collector) rememberPrivateNodes(id string, private map[string]*nodeInfo) {
	now := c.opts.Now().UTC()
	names := make([]string, 0, len(private))
	for name := range private {
		names = append(names, name)
	}
	sort.Strings(names)
	var notes []string
	for _, name := range names {
		n := private[name]
		changed := c.nodeHistory.apply(id, n, now)
		c.nodeHistory.noteChanges(id, n, changed)
		for _, d := range changed {
			notes = append(notes, name+": "+d)
		}
	}
	h := c.nodeHistory
	h.mu.Lock()
	was := h.diverged[id]
	if len(notes) > 0 {
		h.diverged[id] = true
	} else {
		delete(h.diverged, id)
	}
	h.mu.Unlock()
	if c.health == nil {
		return
	}
	src := health.Name("private-nodes", id)
	if len(notes) == 0 {
		if was {
			c.health.OK(src, "private-nodes", id, len(private))
		}
		return
	}
	msg := strings.Join(notes[:min(len(notes), maxDivergedNotes)], "; ")
	if len(notes) > maxDivergedNotes {
		msg += fmt.Sprintf("; and %d more", len(notes)-maxDivergedNotes)
	}
	c.health.Set(src, "private-nodes", id, health.Diverged, msg,
		"a cluster's own nodes are billed as vBilling first saw them, and for no fewer GPUs than they had: their labels and status are written from inside the cluster. "+
			"Bill machines you cannot trust from your machine inventory through the ingest API")
}

// saveNodeHistory writes the private node history if it changed.
func (c *Collector) saveNodeHistory() {
	if err := c.nodeHistory.save(c.opts.Now().UTC()); err != nil {
		log.Printf("[metrics] save private node history: %v (kept in memory, saved again next window)", err)
	}
}
