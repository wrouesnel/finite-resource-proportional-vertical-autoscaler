package frpva_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wrouesnel/ctxstdio"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/entrypoints/frpva"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
)

const (
	operatorNamespace = "frpva-system"
	waitTimeout       = 30 * time.Second
	pollInterval      = 100 * time.Millisecond
)

// cluster is an envtest control plane with the operator running against it.
type cluster struct {
	t      *testing.T
	client client.Client
}

// startCluster starts envtest and runs the operator through its entrypoint, with the webhook using
// the certificates envtest provides. It is skipped if the envtest binaries are not available
// (go run mage.go test provides them).
func startCluster(t *testing.T, policy *v1alpha1.ProportionalResourcePolicy) *cluster {
	t.Helper()
	c, _ := startOperator(t, policy, func(env *envtest.Environment) []string {
		return []string{
			"--webhook-cert-mode", "external",
			"--webhook-cert-dir", env.WebhookInstallOptions.LocalServingCertDir,
			"--webhook-port", strconv.Itoa(env.WebhookInstallOptions.LocalServingPort),
		}
	})
	c.waitForWebhook()
	return c
}

// startOperator starts envtest and runs the operator with the webhook arguments returned by
// webhookArgs.
func startOperator(t *testing.T, policy *v1alpha1.ProportionalResourcePolicy,
	webhookArgs func(*envtest.Environment) []string,
) (*cluster, *envtest.Environment) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: run with go run mage.go test")
	}

	deployDir := filepath.Join("..", "..", "..", "deploy")
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(deployDir, "crd")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{filepath.Join(deployDir, "webhook.yaml")}},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme, err := frpva.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{t: t, client: k8s}

	for _, ns := range []string{operatorNamespace, "excluded-team", "opted-out"} {
		c.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	// Only node-1 runs a kubelet which can resize pod-level resources.
	c.addNode("node-1", "900", "900Gi", "InPlacePodLevelResourcesVerticalScaling")
	c.addNode("node-2", "100", "100Gi")
	c.create(policy)
	// Shipped empty in the manifests, and filled in by the self-managed certificate rotator.
	c.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: "frpva-webhook-cert"}})

	user, err := env.AddUser(envtest.User{Name: "operator", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(kubeconfigPath, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdOut, stdErr := new(bytes.Buffer), new(bytes.Buffer)
	exited := make(chan int)
	args := []string{
		"--config-file", configPath,
		"--log-level", "debug",
		"run",
		"--kubeconfig", kubeconfigPath,
		"--namespace", operatorNamespace,
		"--no-leader-elect",
		"--metrics-bind-address", "0",
		"--health-probe-bind-address", "0",
	}
	args = append(args, webhookArgs(env)...)
	go func() {
		exited <- frpva.Entrypoint(ctxstdio.Set(ctx, stdOut, stdErr, os.Stdin), args)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-exited:
			if code != 0 {
				t.Errorf("operator exited with %d:\n%s", code, stdErr.String())
			}
		case <-time.After(waitTimeout):
			t.Error("operator did not exit")
		}
	})
	return c, env
}

func (c *cluster) create(obj client.Object) {
	c.t.Helper()
	if err := c.client.Create(context.Background(), obj); err != nil {
		c.t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
	}
}

func (c *cluster) addNode(name, cpu, memory string, declaredFeatures ...string) {
	c.t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c.create(node)
	node.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}
	node.Status.Capacity = node.Status.Allocatable
	node.Status.DeclaredFeatures = declaredFeatures
	if err := c.client.Status().Update(context.Background(), node); err != nil {
		c.t.Fatal(err)
	}
	// New nodes are tainted not-ready until the node lifecycle controller (which envtest doesn't
	// run) sees them become ready.
	node.Spec.Taints = nil
	if err := c.client.Update(context.Background(), node); err != nil {
		c.t.Fatal(err)
	}
}

func newPod(namespace, name, cpu, memory string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName:                     "node-1",
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpu),
						corev1.ResourceMemory: resource.MustParse(memory),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
				},
			}},
		},
	}
}

// updatePolicy applies fn to the policy, retrying if the operator updates its status concurrently.
func (c *cluster) updatePolicy(policy *v1alpha1.ProportionalResourcePolicy, fn func(*v1alpha1.ProportionalResourcePolicy)) {
	c.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c.get(policy)
		fn(policy)
		return c.client.Update(context.Background(), policy)
	})
	if err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) get(obj client.Object) {
	c.t.Helper()
	if err := c.client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		c.t.Fatalf("getting %s: %v", obj.GetName(), err)
	}
}

// waitForWebhook creates probe pods until one is mutated.
func (c *cluster) waitForWebhook() {
	c.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(waitTimeout)
	for idx := 0; time.Now().Before(deadline); idx++ {
		probe := newPod("default", "probe-"+strconv.Itoa(idx), "1m", "1Mi")
		c.create(probe)
		_, annotated := probe.Annotations[proportional.AnnotationOriginalResources]
		if err := c.client.Delete(ctx, probe, client.GracePeriodSeconds(0)); err != nil {
			c.t.Fatal(err)
		}
		if annotated {
			// Let the operator see the probe go.
			time.Sleep(time.Second)
			return
		}
		time.Sleep(pollInterval)
	}
	c.t.Fatal("webhook never became ready")
}

func request(pod *corev1.Pod, name corev1.ResourceName) resource.Quantity {
	return pod.Spec.Containers[0].Resources.Requests[name]
}

func limit(pod *corev1.Pod, name corev1.ResourceName) resource.Quantity {
	return pod.Spec.Containers[0].Resources.Limits[name]
}

// expectResources waits for the pod to have the given CPU and memory requests.
func (c *cluster) expectResources(pod *corev1.Pod, cpu, memory string) {
	c.t.Helper()
	wantCPU, wantMemory := resource.MustParse(cpu), resource.MustParse(memory)
	deadline := time.Now().Add(waitTimeout)
	for {
		c.get(pod)
		gotCPU, gotMemory := request(pod, corev1.ResourceCPU), request(pod, corev1.ResourceMemory)
		if gotCPU.Cmp(wantCPU) == 0 && gotMemory.Cmp(wantMemory) == 0 {
			// Memory is guaranteed in these pods, and must stay that way.
			if gotLimit := limit(pod, corev1.ResourceMemory); gotLimit.Cmp(wantMemory) != 0 {
				c.t.Errorf("%s: memory limit %s, want %s", pod.Name, gotLimit.String(), memory)
			}
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: got cpu=%s memory=%s, want cpu=%s memory=%s",
				pod.Name, gotCPU.String(), gotMemory.String(), cpu, memory)
		}
		time.Sleep(pollInterval)
	}
}

// newPodLevelPod is a pod which sets its resources at the pod level only.
func newPodLevelPod(name, node, cpu, memory string) *corev1.Pod {
	pod := newPod("default", name, cpu, memory)
	pod.Spec.NodeName = node
	pod.Spec.Resources = pod.Spec.Containers[0].Resources.DeepCopy()
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	return pod
}

// expectPodLevelResources waits for the pod to have the given pod-level CPU and memory requests,
// and checks its containers stay without requests.
func (c *cluster) expectPodLevelResources(pod *corev1.Pod, cpu, memory string) {
	c.t.Helper()
	wantCPU, wantMemory := resource.MustParse(cpu), resource.MustParse(memory)
	deadline := time.Now().Add(waitTimeout)
	for {
		c.get(pod)
		gotCPU, gotMemory := pod.Spec.Resources.Requests[corev1.ResourceCPU], pod.Spec.Resources.Requests[corev1.ResourceMemory]
		if gotCPU.Cmp(wantCPU) == 0 && gotMemory.Cmp(wantMemory) == 0 {
			if gotLimit := pod.Spec.Resources.Limits[corev1.ResourceMemory]; gotLimit.Cmp(wantMemory) != 0 {
				c.t.Errorf("%s: pod-level memory limit %s, want %s", pod.Name, gotLimit.String(), memory)
			}
			if len(pod.Spec.Containers[0].Resources.Requests) != 0 {
				c.t.Errorf("%s: container requests were added: %v", pod.Name, pod.Spec.Containers[0].Resources.Requests)
			}
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: got pod-level cpu=%s memory=%s, want cpu=%s memory=%s",
				pod.Name, gotCPU.String(), gotMemory.String(), cpu, memory)
		}
		time.Sleep(pollInterval)
	}
}

func defaultPolicy() *v1alpha1.ProportionalResourcePolicy {
	return &v1alpha1.ProportionalResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.ProportionalResourcePolicySpec{
			Resources: []v1alpha1.ResourcePoolSpec{
				{Name: corev1.ResourceCPU},
				{Name: corev1.ResourceMemory},
			},
			ExcludedNamespaces: []string{"kube-*", "excluded-*"},
			Rebalance: v1alpha1.RebalanceSpec{
				MinInterval: &metav1.Duration{Duration: 0},
			},
		},
	}
}

func TestOperator(t *testing.T) {
	c := startCluster(t, defaultPolicy())

	// 1000 cores and 1000Gi less 10% headroom, but no pod can be bigger than a node less headroom.
	a := newPod("default", "a", "1", "1Gi")
	c.create(a)
	c.expectResources(a, "810", "810Gi")
	orig, err := proportional.OriginalResourcesOf(a)
	if err != nil {
		t.Fatal(err)
	}
	if q := orig.Containers["main"].Requests[corev1.ResourceCPU]; q.String() != "1" {
		t.Errorf("recorded cpu: got %s", q.String())
	}

	// b joins with twice the weight: 900 is shared 1:2 between the pods, and a is rebalanced.
	b := newPod("default", "b", "2", "2Gi")
	c.create(b)
	c.expectResources(b, "600", "600Gi")
	c.expectResources(a, "300", "300Gi")

	// Unmanaged pods are left alone, and their requests come out of the shared capacity.
	excluded := newPod("excluded-team", "x", "45", "45Gi")
	c.create(excluded)
	optedOut := newPod("opted-out", "y", "45", "45Gi")
	optedOut.Annotations = map[string]string{proportional.AnnotationExclude: "true"}
	c.create(optedOut)
	for _, p := range []*corev1.Pod{excluded, optedOut} {
		if _, ok := p.Annotations[proportional.AnnotationOriginalResources]; ok {
			t.Errorf("%s/%s was managed", p.Namespace, p.Name)
		}
		c.expectResources(p, "45", "45Gi")
	}
	c.expectResources(a, "270", "270Gi")
	c.expectResources(b, "540", "540Gi")

	policy := &v1alpha1.ProportionalResourcePolicy{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	deadline := time.Now().Add(waitTimeout)
	for {
		c.get(policy)
		ready := meta.IsStatusConditionTrue(policy.Status.Conditions, "Ready")
		if ready && policy.Status.ManagedPods == 2 && policy.Status.UnmanagedPods == 2 && len(policy.Status.Resources) == 2 &&
			policy.Status.Resources[0].Factor == "270.0000" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy status: %+v", policy.Status)
		}
		time.Sleep(pollInterval)
	}
	if cpu := policy.Status.Resources[0]; cpu.Capacity.String() != "900" || cpu.Unmanaged.String() != "90" || cpu.TotalWeight.String() != "3" {
		t.Errorf("cpu status: %+v", cpu)
	}

	// When b goes, a grows into its share (capped at what fits on one node).
	if err := c.client.Delete(context.Background(), b, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	c.expectResources(a, "810", "810Gi")
}

func TestOperatorAdoptsExistingPods(t *testing.T) {
	policy := defaultPolicy()
	c := startCluster(t, policy)
	ctx := context.Background()

	c.updatePolicy(policy, func(p *v1alpha1.ProportionalResourcePolicy) { p.Spec.Suspend = true })
	// Once the operator sees the suspension, pods are admitted as they are.
	var legacy *corev1.Pod
	deadline := time.Now().Add(waitTimeout)
	for idx := 0; ; idx++ {
		legacy = newPod("default", "legacy-"+strconv.Itoa(idx), "1", "1Gi")
		c.create(legacy)
		if _, ok := legacy.Annotations[proportional.AnnotationOriginalResources]; !ok {
			break
		}
		if err := c.client.Delete(ctx, legacy, client.GracePeriodSeconds(0)); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("pods were still managed while suspended")
		}
		time.Sleep(pollInterval)
	}

	c.updatePolicy(policy, func(p *v1alpha1.ProportionalResourcePolicy) {
		p.Spec.Suspend = false
		p.Spec.Rebalance.AdoptExistingPods = true
	})
	c.expectResources(legacy, "810", "810Gi")
	if _, err := proportional.OriginalResourcesOf(legacy); err != nil {
		t.Errorf("adopted pod: %v", err)
	}
}

func TestOperatorSelfManagedCertificates(t *testing.T) {
	certDir := t.TempDir()
	c, _ := startOperator(t, defaultPolicy(), func(*envtest.Environment) []string {
		return []string{"--webhook-cert-mode", "self-managed", "--webhook-cert-dir", certDir, "--webhook-port", "0"}
	})

	// The rotator generates a CA and serving certificate into the secret.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: "frpva-webhook-cert"}}
	deadline := time.Now().Add(waitTimeout)
	for {
		c.get(secret)
		if len(secret.Data["ca.crt"]) > 0 && len(secret.Data["tls.crt"]) > 0 && len(secret.Data["tls.key"]) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("certificate secret was never populated")
		}
		time.Sleep(pollInterval)
	}

	// Stand in for the kubelet mounting the secret into the pod.
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.WriteFile(filepath.Join(certDir, name), secret.Data[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The CA is injected into the webhook configuration.
	webhookConfig := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "frpva"}}
	for {
		c.get(webhookConfig)
		if bytes.Equal(webhookConfig.Webhooks[0].ClientConfig.CABundle, secret.Data["ca.crt"]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CA was never injected into the webhook configuration")
		}
		time.Sleep(pollInterval)
	}
}

func TestOperatorPodLevelResources(t *testing.T) {
	c := startCluster(t, defaultPolicy())

	resizable := newPodLevelPod("resizable", "node-1", "1", "1Gi")
	c.create(resizable)
	c.expectPodLevelResources(resizable, "810", "810Gi")

	// node-2 can't resize pod-level resources, so this pod is only scaled on admission.
	fixed := newPodLevelPod("fixed", "node-2", "1", "1Gi")
	c.create(fixed)
	c.expectPodLevelResources(fixed, "450", "450Gi")
	c.expectPodLevelResources(resizable, "450", "450Gi")

	// A pod with container-level resources joins with twice the weight.
	b := newPod("default", "b", "2", "2Gi")
	c.create(b)
	c.expectResources(b, "450", "450Gi")
	c.expectPodLevelResources(resizable, "225", "225Gi")
	c.expectPodLevelResources(fixed, "450", "450Gi")
}
