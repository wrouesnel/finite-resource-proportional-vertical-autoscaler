// Package webhook implements the mutating admission webhook which records the original resources of
// each new pod and scales them to the pod's proportional share of the cluster.
package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	logutil "github.com/wrouesnel/go.logutil"
	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/snapshot"
)

// Path is the URL path the webhook is served on.
const Path = "/mutate-v1-pod"

// PodMutator scales the resources of pods as they are created.
type PodMutator struct {
	Loader  *snapshot.Loader
	Decoder admission.Decoder
}

// Handle implements admission.Handler.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	l := logutil.FromCtx(ctx).With(zap.String("namespace", req.Namespace), zap.String("name", req.Name),
		zap.String("uid", string(req.UID)))

	if req.Operation != admissionv1.Create || req.SubResource != "" {
		return admission.Allowed("only pod creation is handled")
	}

	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	// The namespace of a pod being created may only be in the request.
	submittedNamespace := pod.Namespace
	pod.Namespace = req.Namespace

	policy, err := m.Loader.Policy(ctx)
	if apierrors.IsNotFound(err) {
		return admission.Allowed("no policy")
	} else if err != nil {
		l.Error("Could not read policy", zap.Error(err))
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if policy.Spec.Suspend {
		return admission.Allowed("policy is suspended")
	}

	c, err := m.Loader.Classifier(policy)
	if err != nil {
		l.Error("Policy is invalid", zap.Error(err))
		return admission.Allowed("policy is invalid")
	}
	if !c.Eligible(pod) {
		return admission.Allowed("pod is not managed")
	}

	// A pod which already carries the annotation has already been scaled: either this is a
	// reinvocation after another webhook changed the pod, or the pod was recreated from the manifest
	// of a managed pod. Its recorded resources are kept as its weights, and only containers added
	// since are recorded.
	orig, err := proportional.OriginalResourcesOf(pod)
	if err != nil {
		orig = proportional.RecordOriginalResources(pod)
	} else {
		orig.RecordMissing(pod)
	}

	state, err := m.Loader.Load(ctx, policy, c)
	if err != nil {
		l.Error("Could not read cluster state", zap.Error(err))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	scaled := proportional.ScalePod(pod, orig, state.Plan.ScaleOptions(c.Weight(pod, orig), true))
	if err := proportional.SetOriginalResources(scaled, orig); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// Only patch the resources and annotation, not the namespace filled in above.
	scaled.Namespace = submittedNamespace

	raw, err := json.Marshal(scaled)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	l.Debug("Scaled pod resources")
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
