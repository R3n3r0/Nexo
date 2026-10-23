package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

// The API server of the tests (envtest: kube-apiserver and etcd, no
// kubelet nor controller manager: the tests play the StatefulSet controller
// and the kubelet by creating the pods and marking them ready).
var (
	k8s       client.Client
	k8sScheme *runtime.Scheme
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set (make operator-test downloads the API server): the envtest tests are skipped")
		os.Exit(m.Run())
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "deploy", "operator", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest:", err)
		os.Exit(1)
	}
	k8sScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(k8sScheme)
	_ = nexov1.AddToScheme(k8sScheme)
	k8s, err = client.New(cfg, client.Options{Scheme: k8sScheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// fakeHealth is the health the tests decide.
type fakeHealth struct {
	mu sync.Mutex
	h  Health
}

func (f *fakeHealth) set(h Health) {
	f.mu.Lock()
	f.h = h
	f.mu.Unlock()
}

func (f *fakeHealth) Check(context.Context, *nexov1.NexoCluster, *corev1.Secret) (Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h, nil
}

func healthy(nodes int) Health {
	return Health{Nodes: nodes, NodesOnline: nodes, Drives: 2 * nodes, DrivesOnline: 2 * nodes, WriteQuorum: true, Maintenance: true}
}

// harness drives the reconciler of one cluster in its own namespace.
type harness struct {
	t      *testing.T
	ctx    context.Context
	ns     string
	r      *Reconciler
	health *fakeHealth
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if k8s == nil {
		t.Skip("envtest is not available")
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "nexo-"}}
	require.NoError(t, k8s.Create(ctx, ns))
	h := &harness{t: t, ctx: ctx, ns: ns.Name, health: &fakeHealth{}}
	h.r = &Reconciler{Client: k8s, Scheme: k8sScheme, Health: h.health}
	return h
}

func (h *harness) secret(name string, data map[string]string) {
	h.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}, StringData: data}
	require.NoError(h.t, k8s.Create(h.ctx, s))
}

func (h *harness) create(pools ...nexov1.PoolSpec) *nexov1.NexoCluster {
	h.t.Helper()
	c := &nexov1.NexoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", Namespace: h.ns},
		Spec: nexov1.NexoClusterSpec{Image: "ghcr.io/r3n3r0/nexo:v1.0.0", Credentials: nexov1.CredentialsSpec{SecretName: "creds"},
			Pools: pools},
	}
	require.NoError(h.t, k8s.Create(h.ctx, c))
	return c
}

func pool(name string, servers int32) nexov1.PoolSpec {
	return nexov1.PoolSpec{Name: name, Servers: servers, VolumesPerServer: 2, Storage: resource.MustParse("1Gi")}
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: h.ns, Name: "lake"}})
	require.NoError(h.t, err)
	return res
}

func (h *harness) cluster() *nexov1.NexoCluster {
	h.t.Helper()
	c := &nexov1.NexoCluster{}
	require.NoError(h.t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake"}, c))
	return c
}

func (h *harness) update(fn func(c *nexov1.NexoCluster)) error {
	c := h.cluster()
	fn(c)
	return k8s.Update(h.ctx, c)
}

// startPods plays the StatefulSet controller and the kubelet: the missing
// pods of every StatefulSet of the cluster are created from its template,
// ready.
func (h *harness) startPods() {
	h.t.Helper()
	list := &appsv1.StatefulSetList{}
	require.NoError(h.t, k8s.List(h.ctx, list, client.InNamespace(h.ns)))
	for _, sts := range list.Items {
		for i := range *sts.Spec.Replicas {
			name := fmt.Sprintf("%s-%d", sts.Name, i)
			if err := k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: name}, &corev1.Pod{}); err == nil {
				continue
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns, Labels: sts.Spec.Template.Labels, Annotations: sts.Spec.Template.Annotations},
				Spec:       sts.Spec.Template.Spec,
			}
			// The drives are claims of the StatefulSet.
			for _, c := range sts.Spec.VolumeClaimTemplates {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: c.Name, VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.Name + "-" + name}}})
			}
			require.NoError(h.t, k8s.Create(h.ctx, pod))
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			require.NoError(h.t, k8s.Status().Update(h.ctx, pod))
		}
	}
}

// pods returns the names of the pods of the cluster, with their template
// hash.
func (h *harness) pods() map[string]string {
	h.t.Helper()
	list := &corev1.PodList{}
	require.NoError(h.t, k8s.List(h.ctx, list, client.InNamespace(h.ns)))
	out := map[string]string{}
	for _, p := range list.Items {
		if p.DeletionTimestamp.IsZero() {
			out[p.Name] = p.Annotations[annotationTemplate]
		}
	}
	return out
}

func (h *harness) condition(typ string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(h.cluster().Status.Conditions, typ)
	if c == nil {
		return metav1.ConditionUnknown
	}
	return c.Status
}

// TestOperatorCreatesCluster (S-OPS-16): a NexoCluster without its Secret
// is invalid; with it the operator creates a StatefulSet per pool (the pods
// replaced by the operator, the claims kept), the headless, S3 and console
// Services and the disruption budget, and reports the cluster Ready once
// the pods are ready and the cluster has its write quorum.
//
// covers: S-OPS-16
func TestOperatorCreatesCluster(t *testing.T) {
	h := newHarness(t)
	h.create(pool("p0", 4))
	h.reconcile()
	c := h.cluster()
	assert.Equal(t, nexov1.PhaseInvalid, c.Status.Phase)
	assert.Contains(t, meta.FindStatusCondition(c.Status.Conditions, nexov1.ConditionDegraded).Message, "Secret creds not found")

	h.secret("creds", map[string]string{"root-user": "admin", "root-password": "secret-password"})
	h.reconcile()
	c = h.cluster()
	assert.Equal(t, nexov1.PhasePending, c.Status.Phase)
	require.Len(t, c.Status.Pools, 1)
	assert.Equal(t, int32(4), c.Status.Pools[0].Servers)

	sts := &appsv1.StatefulSet{}
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake-p0"}, sts))
	assert.Equal(t, appsv1.OnDeleteStatefulSetStrategyType, sts.Spec.UpdateStrategy.Type)
	assert.Equal(t, appsv1.RetainPersistentVolumeClaimRetentionPolicyType, sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted)
	require.Len(t, sts.OwnerReferences, 1)
	assert.Equal(t, "NexoCluster", sts.OwnerReferences[0].Kind)
	for _, name := range []string{"lake", "lake-hl", "lake-console"} {
		require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: name}, &corev1.Service{}), name)
	}
	hl := &corev1.Service{}
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake-hl"}, hl))
	assert.True(t, hl.Spec.PublishNotReadyAddresses)
	pdb := &policyv1.PodDisruptionBudget{}
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake"}, pdb))
	assert.Equal(t, int32(1), pdb.Spec.MaxUnavailable.IntVal)

	h.startPods()
	h.health.set(healthy(4))
	h.reconcile()
	c = h.cluster()
	assert.Equal(t, nexov1.PhaseReady, c.Status.Phase)
	assert.Equal(t, "4/4", c.Status.Ready)
	assert.Equal(t, "ghcr.io/r3n3r0/nexo:v1.0.0", c.Status.Image)
	assert.Equal(t, metav1.ConditionTrue, h.condition(nexov1.ConditionAvailable))
	require.NotNil(t, c.Status.Health)
	assert.True(t, c.Status.Health.WriteQuorum)
}

// TestOperatorRollingUpdate (S-OPS-16): a new image replaces one pod at a
// time: the next one only once every pod is ready again and the cluster can
// lose a node; a cluster that cannot lose a node waits.
//
// covers: S-OPS-16
func TestOperatorRollingUpdate(t *testing.T) {
	h := newHarness(t)
	h.secret("creds", map[string]string{"root-user": "admin", "root-password": "secret-password"})
	h.create(pool("p0", 4))
	h.reconcile()
	h.startPods()
	h.health.set(healthy(4))
	h.reconcile()
	before := h.pods()
	require.Len(t, before, 4)

	require.NoError(t, h.update(func(c *nexov1.NexoCluster) { c.Spec.Image = "ghcr.io/r3n3r0/nexo:v1.1.0" }))
	// The cluster cannot lose a node: nothing is replaced.
	h.health.set(Health{Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 7, WriteQuorum: true})
	h.reconcile()
	assert.Len(t, h.pods(), 4)
	assert.Equal(t, nexov1.PhaseUpdating, h.cluster().Status.Phase)
	assert.Equal(t, "WaitingForQuorum", meta.FindStatusCondition(h.cluster().Status.Conditions, nexov1.ConditionProgressing).Reason)

	h.health.set(healthy(4))
	for i := range 4 {
		h.reconcile()
		pods := h.pods()
		assert.Len(t, pods, 3, "round %d: one pod replaced at a time", i)
		// Until the pod is back, nothing else is replaced.
		h.reconcile()
		assert.Len(t, h.pods(), 3)
		h.startPods()
	}
	h.reconcile()
	c := h.cluster()
	assert.Equal(t, nexov1.PhaseReady, c.Status.Phase)
	assert.Equal(t, "ghcr.io/r3n3r0/nexo:v1.1.0", c.Status.Image)
	for name, hash := range h.pods() {
		assert.NotEqual(t, before[name], hash, "%s runs the new template", name)
	}
	assert.Equal(t, int32(4), c.Status.Pools[0].UpdatedServers)
}

// TestOperatorAllAtOnce (S-OPS-16): the AllAtOnce strategy replaces every
// outdated pod together; so does a new pool (the topology every node must
// share), whatever the strategy.
//
// covers: S-OPS-16
func TestOperatorAllAtOnce(t *testing.T) {
	h := newHarness(t)
	h.secret("creds", map[string]string{"root-user": "admin", "root-password": "secret-password"})
	h.create(pool("p0", 4))
	h.reconcile()
	h.startPods()
	h.health.set(healthy(4))
	h.reconcile()

	// A new pool: the existing pods go together.
	require.NoError(t, h.update(func(c *nexov1.NexoCluster) { c.Spec.Pools = append(c.Spec.Pools, pool("p1", 2)) }))
	h.reconcile()
	assert.Empty(t, h.pods(), "every pod of the old topology replaced together")
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake-p1"}, &appsv1.StatefulSet{}))
	require.Len(t, h.cluster().Status.Pools, 2)
	h.startPods()
	h.health.set(healthy(6))
	h.reconcile()
	assert.Equal(t, nexov1.PhaseReady, h.cluster().Status.Phase)
	assert.Len(t, h.pods(), 6)

	require.NoError(t, h.update(func(c *nexov1.NexoCluster) {
		c.Spec.Image = "ghcr.io/r3n3r0/nexo:v2.0.0"
		c.Spec.UpdateStrategy = nexov1.UpdateAllAtOnce
	}))
	h.reconcile()
	assert.Empty(t, h.pods())
}

// TestOperatorProtectsPools (S-OPS-16): the API refuses to resize a pool;
// the operator refuses to drop a pool that is not confirmed decommissioned,
// and removes a confirmed one by deleting its StatefulSet only (its claims
// stay).
//
// covers: S-OPS-16
func TestOperatorProtectsPools(t *testing.T) {
	h := newHarness(t)
	h.secret("creds", map[string]string{"root-user": "admin", "root-password": "secret-password"})
	h.create(pool("p0", 4), pool("p1", 2))
	h.reconcile()

	err := h.update(func(c *nexov1.NexoCluster) { c.Spec.Pools[1].Servers = 4 })
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "%v", err)
	assert.Contains(t, err.Error(), "never resized")

	require.NoError(t, h.update(func(c *nexov1.NexoCluster) { c.Spec.Pools = c.Spec.Pools[:1] }))
	h.reconcile()
	c := h.cluster()
	assert.Equal(t, nexov1.PhaseInvalid, c.Status.Phase)
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake-p1"}, &appsv1.StatefulSet{}), "kept")

	require.NoError(t, h.update(func(c *nexov1.NexoCluster) {
		c.Annotations = map[string]string{annotationDecommissioned: "p1"}
	}))
	h.reconcile()
	c = h.cluster()
	assert.NotEqual(t, nexov1.PhaseInvalid, c.Status.Phase)
	require.Len(t, c.Status.Pools, 1)
	err = k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: "lake-p1"}, &appsv1.StatefulSet{})
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

// TestSampleIsValid (S-OPS-16): the sample of deploy/operator is accepted by
// the API server (the schema of the CRD) and applied by the operator.
//
// covers: S-OPS-16
func TestSampleIsValid(t *testing.T) {
	h := newHarness(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "operator", "samples", "nexocluster.yaml"))
	require.NoError(t, err)
	var cluster *nexov1.NexoCluster
	for _, doc := range strings.Split(string(data), "\n---\n") {
		obj, _, err := serializer.NewCodecFactory(k8sScheme).UniversalDeserializer().Decode([]byte(doc), nil, nil)
		require.NoError(t, err)
		o := obj.(client.Object)
		o.SetNamespace(h.ns)
		require.NoError(t, k8s.Create(h.ctx, o), "%T", obj)
		if c, ok := obj.(*nexov1.NexoCluster); ok {
			cluster = c
		}
	}
	require.NotNil(t, cluster)
	_, err = h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: h.ns, Name: cluster.Name}})
	require.NoError(t, err)
	c := &nexov1.NexoCluster{}
	require.NoError(t, k8s.Get(h.ctx, types.NamespacedName{Namespace: h.ns, Name: cluster.Name}, c))
	assert.Equal(t, nexov1.PhasePending, c.Status.Phase)
	assert.Equal(t, nexov1.UpdateRolling, c.Spec.UpdateStrategy, "the default of the CRD")
}
