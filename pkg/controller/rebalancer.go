// Package controller implements the controller which keeps running pods in proportion as pods come
// and go and the capacity of the cluster changes.
package controller

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	logutil "github.com/wrouesnel/go.logutil"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/snapshot"
)

const (
	// ConditionReady reports whether the policy is being applied.
	ConditionReady = "Ready"
	// resyncPeriod is how often the cluster is re-examined without any change being seen.
	resyncPeriod = 5 * time.Minute
	// eventReasonResizeFailed is the event recorded on a pod which could not be resized.
	eventReasonResizeFailed = "ProportionalResizeFailed"
)

// resizableResources are the resources which can be changed on a running pod.
//
//nolint:gochecknoglobals
var resizableResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// Rebalancer resizes running pods to their proportional share of the cluster.
type Rebalancer struct {
	Client   client.Client
	Loader   *snapshot.Loader
	Recorder events.EventRecorder
	// Now returns the current time. It is replaceable for tests.
	Now func() time.Time
}

// SetupWithManager registers the controller. Every change to the policy, nodes or pods enqueues
// the single policy the operator acts on.
func (r *Rebalancer) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	toPolicy := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: r.Loader.PolicyName}}}
	})
	err := ctrl.NewControllerManagedBy(mgr).
		Named("rebalancer").
		For(&v1alpha1.ProportionalResourcePolicy{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == r.Loader.PolicyName }),
		)).
		Watches(&corev1.Node{}, toPolicy, builder.WithPredicates(nodeChanged())).
		Watches(&corev1.Pod{}, toPolicy, builder.WithPredicates(podChanged())).
		// The process runs a single manager, except in tests which run the operator repeatedly.
		WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("building rebalancer: %w", err)
	}
	return nil
}

// nodeChanged filters node updates to those which change capacity detection.
func nodeChanged() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldNode, okOld := e.ObjectOld.(*corev1.Node)
		newNode, okNew := e.ObjectNew.(*corev1.Node)
		if !okOld || !okNew {
			return true
		}
		return oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable ||
			!equality.Semantic.DeepEqual(oldNode.Spec.Taints, newNode.Spec.Taints) ||
			!equality.Semantic.DeepEqual(oldNode.Labels, newNode.Labels) ||
			!equality.Semantic.DeepEqual(oldNode.Status.Allocatable, newNode.Status.Allocatable) ||
			!oldNode.DeletionTimestamp.Equal(newNode.DeletionTimestamp)
	}}
}

// podChanged filters pod updates to those which change the proportional calculation.
func podChanged() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldPod, okOld := e.ObjectOld.(*corev1.Pod)
		newPod, okNew := e.ObjectNew.(*corev1.Pod)
		if !okOld || !okNew {
			return true
		}
		return oldPod.Spec.NodeName != newPod.Spec.NodeName ||
			proportional.PodTerminal(oldPod) != proportional.PodTerminal(newPod) ||
			oldPod.Annotations[proportional.AnnotationOriginalResources] != newPod.Annotations[proportional.AnnotationOriginalResources] ||
			oldPod.Annotations[proportional.AnnotationExclude] != newPod.Annotations[proportional.AnnotationExclude] ||
			!equality.Semantic.DeepEqual(oldPod.Spec.Containers, newPod.Spec.Containers) ||
			!equality.Semantic.DeepEqual(oldPod.Spec.InitContainers, newPod.Spec.InitContainers)
	}}
}

// Reconcile implements reconcile.Reconciler.
func (r *Rebalancer) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := logutil.FromCtx(ctx).With(zap.String("policy", req.Name))

	policy, err := r.Loader.Policy(ctx)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, err
	}
	status := policy.Status.DeepCopy()
	status.ObservedGeneration = policy.Generation

	c, err := r.Loader.Classifier(policy)
	if err != nil {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "InvalidSpec", Message: err.Error(),
			ObservedGeneration: policy.Generation,
		})
		return reconcile.Result{}, r.updateStatus(ctx, policy, status)
	}

	state, err := r.Loader.Load(ctx, policy, c)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.Loader.Prune(state)
	setPlanStatus(status, state.Plan)

	result := reconcile.Result{RequeueAfter: resyncPeriod}
	rebalance := policy.Spec.Rebalance
	switch {
	case policy.Spec.Suspend:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "Suspended",
			Message: "The policy is suspended", ObservedGeneration: policy.Generation,
		})
		return result, r.updateStatus(ctx, policy, status)
	case rebalance.ModeOrDefault() == v1alpha1.RebalanceModeDisabled:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "AdmissionOnly",
			Message: "Pods are scaled when admitted, running pods are not rebalanced", ObservedGeneration: policy.Generation,
		})
		return result, r.updateStatus(ctx, policy, status)
	}

	if rebalance.AdoptExistingPods {
		adopted, err := r.adopt(ctx, state)
		if err != nil {
			return reconcile.Result{}, err
		}
		if adopted > 0 {
			l.Info("Adopted existing pods", zap.Int("pods", adopted))
			// The adopted pods change the plan, so start again with them included.
			return reconcile.Result{RequeueAfter: time.Second}, r.updateStatus(ctx, policy, status)
		}
	}

	resizes := r.pendingResizes(state, rebalance.TolerancePercentOrDefault())
	if len(resizes) > 0 {
		if last := status.LastRebalanceTime; last != nil {
			if wait := last.Add(rebalance.MinIntervalOrDefault()).Sub(r.Now()); wait > 0 {
				result.RequeueAfter = wait
				return result, r.updateStatus(ctx, policy, status)
			}
		}
		failed := r.resize(ctx, resizes)
		l.Info("Rebalanced pods", zap.Int("resized", len(resizes)-failed), zap.Int("failed", failed))
		status.LastRebalanceTime = &metav1.Time{Time: r.Now()}
	}

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Rebalancing",
		Message: "Pods are scaled when admitted and running pods are rebalanced", ObservedGeneration: policy.Generation,
	})
	return result, r.updateStatus(ctx, policy, status)
}

// updateStatus writes the status if it has changed.
func (r *Rebalancer) updateStatus(ctx context.Context, policy *v1alpha1.ProportionalResourcePolicy, status *v1alpha1.ProportionalResourcePolicyStatus) error {
	if equality.Semantic.DeepEqual(&policy.Status, status) {
		return nil
	}
	updated := policy.DeepCopy()
	updated.Status = *status
	if err := r.Client.Status().Patch(ctx, updated, client.MergeFrom(policy)); err != nil {
		return fmt.Errorf("updating policy status: %w", err)
	}
	return nil
}

// setPlanStatus reports the plan in the status.
func setPlanStatus(status *v1alpha1.ProportionalResourcePolicyStatus, plan *proportional.Plan) {
	status.CountedNodes = clampInt32(plan.CountedNodes)
	status.ManagedPods = clampInt32(plan.ManagedPods)
	status.UnmanagedPods = clampInt32(plan.UnmanagedPods)
	status.Resources = make([]v1alpha1.ResourcePoolStatus, 0, len(plan.Pools))
	for _, pool := range plan.Pools {
		name := pool.Spec.Name
		status.Resources = append(status.Resources, v1alpha1.ResourcePoolStatus{
			Name:            name,
			NodeAllocatable: proportional.FromUnits(name, pool.NodeAllocatable),
			Capacity:        proportional.FromUnits(name, pool.Capacity),
			Unmanaged:       proportional.FromUnits(name, pool.Unmanaged),
			Available:       proportional.FromUnits(name, pool.Available),
			TotalWeight:     proportional.FromUnits(name, pool.Weight),
			Factor:          pool.Factor(0).FloatString(4),
		})
	}
}

// adopt annotates eligible pods which were never admitted by the webhook with their current
// resources, which makes them managed.
func (r *Rebalancer) adopt(ctx context.Context, state *snapshot.Snapshot) (int, error) {
	adopted := 0
	for i := range state.Pods {
		entry := &state.Pods[i]
		pod := entry.Pod
		if entry.Managed() || !state.Classifier.Eligible(pod) || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if _, hasAnnotation := pod.Annotations[proportional.AnnotationOriginalResources]; hasAnnotation {
			// An annotation which doesn't decode is left for a human to look at.
			continue
		}
		updated := pod.DeepCopy()
		if err := proportional.SetOriginalResources(updated, proportional.RecordOriginalResources(pod)); err != nil {
			return adopted, err
		}
		if err := r.Client.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return adopted, fmt.Errorf("adopting pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		adopted++
	}
	return adopted, nil
}

// pendingResize is a pod which has drifted out of proportion.
type pendingResize struct {
	current *corev1.Pod
	desired *corev1.Pod
}

// pendingResizes finds the managed pods whose resizable requests are further than tolerancePercent
// from their proportional values.
func (r *Rebalancer) pendingResizes(state *snapshot.Snapshot, tolerancePercent int32) []pendingResize {
	var resizes []pendingResize
	for i := range state.Pods {
		entry := &state.Pods[i]
		pod := entry.Pod
		// Pod-level resources can't be resized in place, so such pods are only scaled on admission.
		if !entry.Managed() || !pod.DeletionTimestamp.IsZero() || pod.Spec.Resources != nil {
			continue
		}
		opts := state.Plan.ScaleOptions(state.Classifier.EntryWeight(entry), false)
		scaled := proportional.ScalePod(pod, entry.Original, opts)
		desired := resizeTarget(pod, scaled)
		if outOfTolerance(pod, desired, tolerancePercent) {
			resizes = append(resizes, pendingResize{current: pod, desired: desired})
		}
	}
	return resizes
}

// resizeTarget is the pod with only the resizable resources of the regular and sidecar containers
// taken from scaled.
func resizeTarget(pod, scaled *corev1.Pod) *corev1.Pod {
	out := pod.DeepCopy()
	copyResizable := func(dst, src []corev1.Container, filter func(*corev1.Container) bool) {
		for i := range dst {
			if !filter(&dst[i]) {
				continue
			}
			for _, name := range resizableResources {
				copyResource(&dst[i].Resources.Requests, src[i].Resources.Requests, name)
				copyResource(&dst[i].Resources.Limits, src[i].Resources.Limits, name)
			}
		}
	}
	copyResizable(out.Spec.Containers, scaled.Spec.Containers, func(*corev1.Container) bool { return true })
	copyResizable(out.Spec.InitContainers, scaled.Spec.InitContainers, isSidecar)
	return out
}

func isSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func copyResource(dst *corev1.ResourceList, src corev1.ResourceList, name corev1.ResourceName) {
	q, ok := src[name]
	if !ok {
		return
	}
	if *dst == nil {
		*dst = corev1.ResourceList{}
	}
	(*dst)[name] = q
}

// outOfTolerance reports whether any resizable request of pod is further than tolerancePercent from
// the request of desired.
func outOfTolerance(pod, desired *corev1.Pod, tolerancePercent int32) bool {
	for i := range pod.Spec.Containers {
		if containerOutOfTolerance(&pod.Spec.Containers[i], &desired.Spec.Containers[i], tolerancePercent) {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if isSidecar(&pod.Spec.InitContainers[i]) &&
			containerOutOfTolerance(&pod.Spec.InitContainers[i], &desired.Spec.InitContainers[i], tolerancePercent) {
			return true
		}
	}
	return false
}

func containerOutOfTolerance(current, desired *corev1.Container, tolerancePercent int32) bool {
	for _, name := range resizableResources {
		want, ok := desired.Resources.Requests[name]
		if !ok {
			continue
		}
		have := current.Resources.Requests[name]
		w, h := proportional.ToUnits(name, want), proportional.ToUnits(name, have)
		// |h - w| / w > tolerance
		drift := new(big.Rat).Abs(big.NewRat(h-w, max(w, 1)))
		if drift.Cmp(big.NewRat(int64(tolerancePercent), 100)) > 0 {
			return true
		}
	}
	return false
}

// resize applies the pending resizes in two passes: first every resource which is shrinking, and
// only then every resource which is growing, so that growing pods have room to grow into.
func (r *Rebalancer) resize(ctx context.Context, resizes []pendingResize) int {
	failed := map[*corev1.Pod]struct{}{}
	shrunk := make([]*corev1.Pod, len(resizes))
	for i, rs := range resizes {
		shrunk[i] = minResources(rs.current, rs.desired)
		if equality.Semantic.DeepEqual(shrunk[i].Spec, rs.current.Spec) {
			shrunk[i] = rs.current
			continue
		}
		if err := r.patchResize(ctx, rs.current, shrunk[i]); err != nil {
			failed[rs.current] = struct{}{}
		}
	}
	for i, rs := range resizes {
		if _, ok := failed[rs.current]; ok {
			continue
		}
		if equality.Semantic.DeepEqual(shrunk[i].Spec, rs.desired.Spec) {
			continue
		}
		if err := r.patchResize(ctx, shrunk[i], rs.desired); err != nil {
			failed[rs.current] = struct{}{}
		}
	}
	return len(failed)
}

// patchResize patches the resources of from to those of to through the resize subresource.
func (r *Rebalancer) patchResize(ctx context.Context, from, to *corev1.Pod) error {
	l := logutil.FromCtx(ctx).With(zap.String("namespace", from.Namespace), zap.String("name", from.Name))
	err := r.Client.SubResource("resize").Patch(ctx, to.DeepCopy(), client.StrategicMergeFrom(from))
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	l.Warn("Could not resize pod", zap.Error(err))
	if r.Recorder != nil {
		r.Recorder.Eventf(from, nil, corev1.EventTypeWarning, eventReasonResizeFailed, "Resize",
			"Could not resize to proportional share: %v", err)
	}
	return err
}

// minResources is desired with each resizable request and limit no larger than in current.
func minResources(current, desired *corev1.Pod) *corev1.Pod {
	out := desired.DeepCopy()
	lower := func(dst []corev1.Container, src []corev1.Container) {
		for i := range dst {
			for _, name := range resizableResources {
				lowerResource(dst[i].Resources.Requests, src[i].Resources.Requests, name)
				lowerResource(dst[i].Resources.Limits, src[i].Resources.Limits, name)
			}
		}
	}
	lower(out.Spec.Containers, current.Spec.Containers)
	lower(out.Spec.InitContainers, current.Spec.InitContainers)
	return out
}

func lowerResource(dst, src corev1.ResourceList, name corev1.ResourceName) {
	want, ok := dst[name]
	if !ok {
		return
	}
	if have, ok := src[name]; ok && have.Cmp(want) < 0 {
		dst[name] = have
	}
}

// clampInt32 converts a count for reporting in the status.
func clampInt32(v int) int32 {
	return int32(min(v, math.MaxInt32)) //nolint:gosec // clamped above
}
