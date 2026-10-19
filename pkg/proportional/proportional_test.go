package proportional_test

import (
	"math/big"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
)

func resources(cpu, memory string) corev1.ResourceList {
	list := corev1.ResourceList{}
	if cpu != "" {
		list[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		list[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return list
}

func node(name, cpu, memory string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Allocatable: resources(cpu, memory)},
	}
}

func pod(namespace, name string, requests, limits corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(namespace + "/" + name)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "main",
			Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits},
		}}},
	}
}

// managed returns the pod as admitted: annotated with its original resources.
func managed(t *testing.T, p *corev1.Pod) *corev1.Pod {
	t.Helper()
	if err := proportional.SetOriginalResources(p, proportional.RecordOriginalResources(p)); err != nil {
		t.Fatal(err)
	}
	return p
}

func cpuMemSpec() *v1alpha1.ProportionalResourcePolicySpec {
	return &v1alpha1.ProportionalResourcePolicySpec{
		Resources:          []v1alpha1.ResourcePoolSpec{{Name: corev1.ResourceCPU}, {Name: corev1.ResourceMemory}},
		ExcludedNamespaces: []string{"kube-*"},
	}
}

func buildPlan(t *testing.T, spec *v1alpha1.ProportionalResourcePolicySpec, nodes []corev1.Node, pods ...*corev1.Pod) (*proportional.Classifier, *proportional.Plan) {
	t.Helper()
	c, err := proportional.NewClassifier(spec, "frpva-system")
	if err != nil {
		t.Fatal(err)
	}
	list := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		list = append(list, *p)
	}
	entries := proportional.NewEntryCache().Entries(c, list)
	return c, proportional.BuildPlan(c, spec, nodes, entries)
}

func pool(t *testing.T, plan *proportional.Plan, name corev1.ResourceName) *proportional.Pool {
	t.Helper()
	for _, p := range plan.Pools {
		if p.Spec.Name == name {
			return p
		}
	}
	t.Fatalf("no pool for %s", name)
	return nil
}

func requestOf(p *corev1.Pod, name corev1.ResourceName) string {
	q := p.Spec.Containers[0].Resources.Requests[name]
	return q.String()
}

func limitOf(p *corev1.Pod, name corev1.ResourceName) string {
	q := p.Spec.Containers[0].Resources.Limits[name]
	return q.String()
}

func TestAnnotationRoundTrip(t *testing.T) {
	p := pod("default", "a", resources("100m", "64Mi"), resources("200m", "128Mi"))
	p.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{Requests: resources("50m", "")}}}
	managed(t, p)

	orig, err := proportional.OriginalResourcesOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if q := orig.Containers["main"].Limits[corev1.ResourceMemory]; q.String() != "128Mi" {
		t.Errorf("main memory limit: got %s", q.String())
	}
	if q := orig.InitContainers["init"].Requests[corev1.ResourceCPU]; q.String() != "50m" {
		t.Errorf("init cpu request: got %s", q.String())
	}

	// Scaling and then restoring gets back the original pod.
	scaled := proportional.ScalePod(p, orig, &proportional.ScaleOptions{
		Factors: map[corev1.ResourceName]*big.Rat{corev1.ResourceCPU: big.NewRat(3, 1)},
	})
	if got := requestOf(scaled, corev1.ResourceCPU); got != "300m" {
		t.Errorf("scaled cpu: got %s", got)
	}
	if got := requestOf(proportional.OriginalPod(scaled, orig), corev1.ResourceCPU); got != "100m" {
		t.Errorf("restored cpu: got %s", got)
	}
}

func TestScaleUnits(t *testing.T) {
	cases := []struct {
		name   corev1.ResourceName
		value  int64
		factor *big.Rat
		want   int64
	}{
		{corev1.ResourceCPU, 100, big.NewRat(1, 3), 33},
		{corev1.ResourceCPU, 100, big.NewRat(2, 3), 67},
		{corev1.ResourceCPU, 1, big.NewRat(1, 1000), 1},
		{corev1.ResourceMemory, 1 << 20, big.NewRat(1, 2), 1 << 19},
		{corev1.ResourceMemory, 1000, big.NewRat(1, 1), 1024},
		{corev1.ResourceMemory, 1 << 40, big.NewRat(1<<20, 1), 1 << 60},
	}
	for _, tc := range cases {
		if got := proportional.ScaleUnits(tc.name, tc.value, tc.factor); got != tc.want {
			t.Errorf("ScaleUnits(%s, %d, %s): got %d, want %d", tc.name, tc.value, tc.factor, got, tc.want)
		}
	}
}

func TestPlanSharesCapacityProportionally(t *testing.T) {
	nodes := []corev1.Node{node("n1", "10", "10Gi"), node("n2", "10", "10Gi")}
	a := managed(t, pod("default", "a", resources("100m", "1Gi"), nil))
	b := managed(t, pod("default", "b", resources("300m", "3Gi"), nil))
	c, plan := buildPlan(t, cpuMemSpec(), nodes, a, b)

	cpu := pool(t, plan, corev1.ResourceCPU)
	// 20 cores less 10% headroom.
	if cpu.Capacity != 18000 || cpu.Available != 18000 || cpu.Weight != 400 {
		t.Errorf("cpu pool: %+v", cpu)
	}
	// No pod may exceed the largest node less headroom.
	if cpu.MaxPod != 9000 {
		t.Errorf("cpu max pod: got %d", cpu.MaxPod)
	}

	for _, tc := range []struct {
		pod      *corev1.Pod
		cpu, mem string
	}{
		{a, "4500m", "4608Mi"},
		{b, "9", "9Gi"},
	} {
		orig, _ := proportional.OriginalResourcesOf(tc.pod)
		scaled := proportional.ScalePod(tc.pod, orig, plan.ScaleOptions(c.Weight(tc.pod, orig), false))
		if got := requestOf(scaled, corev1.ResourceCPU); got != tc.cpu {
			t.Errorf("%s cpu: got %s, want %s", tc.pod.Name, got, tc.cpu)
		}
		if got := requestOf(scaled, corev1.ResourceMemory); got != tc.mem {
			t.Errorf("%s memory: got %s, want %s", tc.pod.Name, got, tc.mem)
		}
	}
}

func TestPlanCapsPodsAtLargestNode(t *testing.T) {
	nodes := []corev1.Node{node("n1", "4", "4Gi"), node("n2", "4", "4Gi")}
	a := managed(t, pod("default", "a", resources("100m", ""), nil))
	c, plan := buildPlan(t, cpuMemSpec(), nodes, a)

	orig, _ := proportional.OriginalResourcesOf(a)
	scaled := proportional.ScalePod(a, orig, plan.ScaleOptions(c.Weight(a, orig), false))
	// Alone, the pod would get 7.2 cores, but it can't be scheduled beyond 3.6.
	if got := requestOf(scaled, corev1.ResourceCPU); got != "3600m" {
		t.Errorf("cpu: got %s", got)
	}
}

func TestPlanJoiningPodIsCounted(t *testing.T) {
	nodes := []corev1.Node{node("n1", "100", "")}
	a := managed(t, pod("default", "a", resources("1", ""), nil))
	c, plan := buildPlan(t, cpuMemSpec(), nodes, a)

	b := pod("default", "b", resources("1", ""), nil)
	orig := proportional.RecordOriginalResources(b)
	scaled := proportional.ScalePod(b, orig, plan.ScaleOptions(c.Weight(b, orig), true))
	// Two equal pods share 90 cores.
	if got := requestOf(scaled, corev1.ResourceCPU); got != "45" {
		t.Errorf("cpu: got %s", got)
	}
}

func TestPlanUnmanagedAndExcluded(t *testing.T) {
	nodes := []corev1.Node{
		node("n1", "10", ""),
		node("n2", "10", ""),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "control-plane"},
			Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}},
			Status:     corev1.NodeStatus{Allocatable: resources("10", "")},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cordoned"},
			Spec:       corev1.NodeSpec{Unschedulable: true},
			Status:     corev1.NodeStatus{Allocatable: resources("10", "")},
		},
	}
	system := pod("kube-system", "dns", resources("2", ""), nil)
	system.Spec.NodeName = "n1"
	onControlPlane := pod("kube-system", "apiserver", resources("2", ""), nil)
	onControlPlane.Spec.NodeName = "control-plane"
	optedOut := managed(t, pod("default", "opted-out", resources("1", ""), nil))
	optedOut.Annotations[proportional.AnnotationExclude] = "true"
	optedOut.Spec.NodeName = "n2"
	operator := managed(t, pod("frpva-system", "operator", resources("1", ""), nil))
	operator.Spec.NodeName = "n2"
	finished := managed(t, pod("default", "finished", resources("1", ""), nil))
	finished.Status.Phase = corev1.PodSucceeded
	a := managed(t, pod("default", "a", resources("1", ""), nil))

	_, plan := buildPlan(t, cpuMemSpec(), nodes, system, onControlPlane, optedOut, operator, finished, a)
	cpu := pool(t, plan, corev1.ResourceCPU)
	if plan.CountedNodes != 2 || plan.ManagedPods != 1 || plan.UnmanagedPods != 3 {
		t.Errorf("plan: %+v", plan)
	}
	// 18 cores, less the 4 requested by unmanaged pods on counted nodes.
	if cpu.Capacity != 18000 || cpu.Unmanaged != 4000 || cpu.Available != 14000 || cpu.Weight != 1000 {
		t.Errorf("cpu pool: %+v", cpu)
	}

	spec := cpuMemSpec()
	spec.SubtractUnmanaged = ptr.To(false)
	spec.Resources[0].Capacity = ptr.To(resource.MustParse("50"))
	_, plan = buildPlan(t, spec, nodes, system, a)
	if cpu := pool(t, plan, corev1.ResourceCPU); cpu.Available != 50000 || cpu.MaxPod != 9000 {
		t.Errorf("configured cpu pool: %+v", cpu)
	}
}

func TestInitContainersAndSidecars(t *testing.T) {
	nodes := []corev1.Node{node("n1", "100", "")}
	p := pod("default", "a", resources("1", ""), nil)
	p.Spec.InitContainers = []corev1.Container{
		{Name: "sidecar", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), Resources: corev1.ResourceRequirements{Requests: resources("1", "")}},
		{Name: "setup", Resources: corev1.ResourceRequirements{Requests: resources("8", "")}},
	}
	managed(t, p)
	c, plan := buildPlan(t, cpuMemSpec(), nodes, p)

	// The effective request is max(setup + sidecar, main + sidecar) = 9 cores.
	if cpu := pool(t, plan, corev1.ResourceCPU); cpu.Weight != 9000 {
		t.Errorf("weight: got %d", cpu.Weight)
	}
	orig, _ := proportional.OriginalResourcesOf(p)
	scaled := proportional.ScalePod(p, orig, plan.ScaleOptions(c.Weight(p, orig), false))
	// 90 cores / 9 = factor 10, capped by the 90 core node at the same factor.
	for _, tc := range []struct {
		container corev1.Container
		want      string
	}{{scaled.Spec.Containers[0], "10"}, {scaled.Spec.InitContainers[0], "10"}, {scaled.Spec.InitContainers[1], "80"}} {
		if q := tc.container.Resources.Requests[corev1.ResourceCPU]; q.String() != tc.want {
			t.Errorf("%s: got %s, want %s", tc.container.Name, q.String(), tc.want)
		}
	}
}

func TestLimitPolicies(t *testing.T) {
	nodes := []corev1.Node{node("n1", "10", "10Gi")}
	guaranteed := managed(t, pod("default", "g", resources("1", "1Gi"), resources("1", "1Gi")))
	burstable := managed(t, pod("default", "b", resources("1", "1Gi"), resources("2", "4Gi")))
	spec := cpuMemSpec()
	// Give the pods a quarter of what they asked for.
	spec.Resources[0].Capacity = ptr.To(resource.MustParse("500m"))
	spec.Resources[1].Capacity = ptr.To(resource.MustParse("512Mi"))
	spec.Resources[1].MinRequest = ptr.To(resource.MustParse("300Mi"))

	c, plan := buildPlan(t, spec, nodes, guaranteed, burstable)
	scale := func(p *corev1.Pod) *corev1.Pod {
		orig, _ := proportional.OriginalResourcesOf(p)
		return proportional.ScalePod(p, orig, plan.ScaleOptions(c.Weight(p, orig), false))
	}

	g := scale(guaranteed)
	if requestOf(g, corev1.ResourceCPU) != "250m" || limitOf(g, corev1.ResourceCPU) != "250m" {
		t.Errorf("guaranteed cpu: %v", g.Spec.Containers[0].Resources)
	}
	// The minimum request applies, and the limit follows so the pod stays guaranteed.
	if requestOf(g, corev1.ResourceMemory) != "300Mi" || limitOf(g, corev1.ResourceMemory) != "300Mi" {
		t.Errorf("guaranteed memory: %v", g.Spec.Containers[0].Resources)
	}
	b := scale(burstable)
	if requestOf(b, corev1.ResourceCPU) != "250m" || limitOf(b, corev1.ResourceCPU) != "500m" {
		t.Errorf("burstable cpu: %v", b.Spec.Containers[0].Resources)
	}

	plan.LimitPolicy = v1alpha1.LimitPolicyUnchanged
	b = scale(burstable)
	if requestOf(b, corev1.ResourceCPU) != "250m" || limitOf(b, corev1.ResourceCPU) != "2" {
		t.Errorf("unchanged burstable cpu: %v", b.Spec.Containers[0].Resources)
	}
}

func TestPodsWithoutRequestsAreUntouched(t *testing.T) {
	nodes := []corev1.Node{node("n1", "10", "10Gi")}
	bestEffort := managed(t, pod("default", "be", nil, nil))
	cpuOnly := managed(t, pod("default", "cpu", resources("1", ""), nil))
	c, plan := buildPlan(t, cpuMemSpec(), nodes, bestEffort, cpuOnly)

	for _, p := range []*corev1.Pod{bestEffort, cpuOnly} {
		orig, _ := proportional.OriginalResourcesOf(p)
		scaled := proportional.ScalePod(p, orig, plan.ScaleOptions(c.Weight(p, orig), false))
		if _, ok := scaled.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]; ok {
			t.Errorf("%s: memory request was added", p.Name)
		}
	}
}

func TestInvalidAnnotationIsUnmanaged(t *testing.T) {
	nodes := []corev1.Node{node("n1", "10", "")}
	p := pod("default", "a", resources("1", ""), nil)
	p.Annotations = map[string]string{proportional.AnnotationOriginalResources: "{not json"}
	p.Spec.NodeName = "n1"
	_, plan := buildPlan(t, cpuMemSpec(), nodes, p)
	if plan.ManagedPods != 0 || plan.UnmanagedPods != 1 {
		t.Errorf("plan: %+v", plan)
	}
}

func TestInvalidExcludedNamespacePattern(t *testing.T) {
	spec := cpuMemSpec()
	spec.ExcludedNamespaces = []string{"[unterminated"}
	if _, err := proportional.NewClassifier(spec, ""); err == nil {
		t.Error("expected an error")
	}
}
