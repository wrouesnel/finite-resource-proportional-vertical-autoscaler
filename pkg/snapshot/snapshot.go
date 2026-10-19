// Package snapshot reads the current proportional scaling state of the cluster. It is shared by the
// admission webhook and the rebalancing controller so both see the cluster the same way.
package snapshot

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
)

// Loader reads snapshots of the cluster.
type Loader struct {
	// Reader should be backed by the manager cache.
	Reader client.Reader
	// PolicyName is the name of the ProportionalResourcePolicy which is acted on.
	PolicyName string
	// OperatorNamespace is excluded from management.
	OperatorNamespace string
	// Entries caches decoded pod annotations.
	Entries *proportional.EntryCache
}

// Snapshot is the state of the cluster at a point in time. The pods it holds are shared with the
// cache and must not be modified.
type Snapshot struct {
	Policy     *v1alpha1.ProportionalResourcePolicy
	Classifier *proportional.Classifier
	Plan       *proportional.Plan
	Pods       []proportional.PodEntry
}

// Policy fetches the policy.
func (l *Loader) Policy(ctx context.Context) (*v1alpha1.ProportionalResourcePolicy, error) {
	policy := &v1alpha1.ProportionalResourcePolicy{}
	if err := l.Reader.Get(ctx, types.NamespacedName{Name: l.PolicyName}, policy); err != nil {
		return nil, fmt.Errorf("getting policy %s: %w", l.PolicyName, err)
	}
	return policy, nil
}

// Classifier builds the classifier of the policy.
func (l *Loader) Classifier(policy *v1alpha1.ProportionalResourcePolicy) (*proportional.Classifier, error) {
	return proportional.NewClassifier(&policy.Spec, l.OperatorNamespace)
}

// Load reads the cluster state under the given policy.
func (l *Loader) Load(ctx context.Context, policy *v1alpha1.ProportionalResourcePolicy, c *proportional.Classifier) (*Snapshot, error) {
	nodes := &corev1.NodeList{}
	if err := l.Reader.List(ctx, nodes, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	pods := &corev1.PodList{}
	if err := l.Reader.List(ctx, pods, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	entries := l.Entries.Entries(c, pods.Items)
	return &Snapshot{
		Policy:     policy,
		Classifier: c,
		Plan:       proportional.BuildPlan(c, &policy.Spec, nodes.Items, entries),
		Pods:       entries,
	}, nil
}

// Prune drops cache entries of pods which are not in the snapshot.
func (l *Loader) Prune(s *Snapshot) {
	uids := make(map[types.UID]struct{}, len(s.Pods))
	for i := range s.Pods {
		uids[s.Pods[i].Pod.UID] = struct{}{}
	}
	l.Entries.Retain(uids)
}
