package metrics

import "fmt"

// seriesFilter uses namespace AND pod, so identically named pods in two
// namespaces never inherit each other's ownership. Missing labels fail the
// source before any baselines or quantities are advanced.
func (v *syncedView) seriesFilter(t Target, elsewhere []string) (func(map[string]string) (bool, error), bool) {
	if v == nil {
		return nil, false
	}
	in := map[string]bool{t.Cluster.Namespace: true}
	for _, ns := range elsewhere {
		in[ns] = true
	}
	needed := false
	for ns := range in {
		needed = needed || v.contaminated[ns]
	}
	if !needed {
		return nil, false
	}
	id := t.Cluster.ExternalID()
	return func(labels map[string]string) (bool, error) {
		ns := labels["namespace"]
		if ns == "" {
			return false, fmt.Errorf("counter in a namespace shared by clusters has no namespace label; ownership cannot be determined")
		}
		if !in[ns] {
			return false, nil
		}
		if !v.contaminated[ns] {
			return true, nil
		}
		pod := labels["pod"]
		if pod == "" {
			return false, fmt.Errorf("counter in namespace %s has no pod label; another cluster syncs pods here, so ownership cannot be determined", ns)
		}
		owner, known := v.podOwners[ns+"\x00"+pod]
		if !known {
			return false, fmt.Errorf("counter for pod %s/%s has no retained ownership evidence in a namespace shared by clusters", ns, pod)
		}
		if owner == "" {
			return false, fmt.Errorf("counter for pod %s/%s has ambiguous ownership", ns, pod)
		}
		return owner == id, nil
	}, true
}
