// Package controller reconciles the NexoCluster resources: the StatefulSets
// of the pools, the Services, the PodDisruptionBudget, and the replacement of
// the pods whose template changed, one at a time while the cluster can lose a
// node.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

// Requeue delays.
const (
	// waitRequeue paces a rollout or a start (pods coming up).
	waitRequeue = 5 * time.Second
	// resyncRequeue refreshes the status of a stable cluster.
	resyncRequeue = 30 * time.Second
	// invalidRequeue retries a spec that cannot be applied (a missing
	// Secret may appear).
	invalidRequeue = time.Minute
)

// Reconciler reconciles the NexoCluster resources.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Health   HealthChecker
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=nexo.r3n3r0.github.io,resources=nexoclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nexo.r3n3r0.github.io,resources=nexoclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nexov1.NexoCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// The pods belong to the StatefulSets: their changes (ready, gone)
		// pace the rollouts.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
			name, ok := o.GetLabels()[labelCluster]
			if !ok {
				return nil
			}
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
		})).
		Named("nexocluster").
		Complete(r)
}

// errInvalid is a spec the operator cannot apply.
var errInvalid = errors.New("invalid spec")

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &nexov1.NexoCluster{}
	if err := r.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !c.DeletionTimestamp.IsZero() {
		// The owned objects are garbage collected; the claims of the drives
		// stay.
		return ctrl.Result{}, nil
	}
	res, err := r.reconcile(ctx, c)
	if errors.Is(err, errInvalid) {
		r.setPhase(c, nexov1.PhaseInvalid)
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: nexov1.ConditionDegraded, Status: metav1.ConditionTrue,
			Reason: "InvalidSpec", Message: err.Error(), ObservedGeneration: c.Generation})
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: nexov1.ConditionProgressing, Status: metav1.ConditionFalse,
			Reason: "InvalidSpec", Message: "the spec cannot be applied", ObservedGeneration: c.Generation})
		res, err = ctrl.Result{RequeueAfter: invalidRequeue}, nil
	}
	if serr := r.Status().Update(ctx, c); serr != nil && !apierrors.IsConflict(serr) {
		return res, errors.Join(err, serr)
	} else if apierrors.IsConflict(serr) && err == nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return res, err
}

// reconcile applies the spec and updates c.Status (saved by the caller).
func (r *Reconciler) reconcile(ctx context.Context, c *nexov1.NexoCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	removed, err := validate(c)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %w", errInvalid, err)
	}
	creds, err := r.secret(ctx, c, c.Spec.Credentials.SecretName, orDefault(c.Spec.Credentials.UserKey, defaultUserKey),
		orDefault(c.Spec.Credentials.PasswordKey, defaultPasswordKey))
	if err != nil {
		return ctrl.Result{}, err
	}
	var kms, certs *corev1.Secret
	if c.Spec.KMS != nil {
		if kms, err = r.secret(ctx, c, c.Spec.KMS.SecretName, orDefault(c.Spec.KMS.Key, defaultKMSKey)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if c.Spec.TLS != nil {
		if certs, err = r.secret(ctx, c, c.Spec.TLS.SecretName, corev1.TLSCertKey, corev1.TLSPrivateKeyKey); err != nil {
			return ctrl.Result{}, err
		}
	}
	shared := sharedHash(c, creds, kms)

	if err := r.apply(ctx, c, headlessService(c)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, c, service(c, c.Name, "s3", portS3)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, c, service(c, consoleName(c), "console", portConsole)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, c, disruptionBudget(c)); err != nil {
		return ctrl.Result{}, err
	}
	desired := map[string]string{}
	for _, p := range c.Spec.Pools {
		sts := statefulSet(c, p, shared)
		desired[p.Name] = sts.Spec.Template.Annotations[annotationTemplate]
		if err := r.apply(ctx, c, sts); err != nil {
			return ctrl.Result{}, err
		}
		if !slices.ContainsFunc(c.Status.Pools, func(s nexov1.PoolStatus) bool { return s.Name == p.Name }) {
			c.Status.Pools = append(c.Status.Pools, nexov1.PoolStatus{Name: p.Name, Servers: p.Servers, VolumesPerServer: p.VolumesPerServer})
			logger.Info("pool created", "pool", p.Name, "servers", p.Servers, "volumesPerServer", p.VolumesPerServer)
		}
	}

	for _, p := range removed {
		// A decommissioned pool: its StatefulSet goes, its claims stay.
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Name + "-" + p.Name}}
		if err := r.Delete(ctx, sts); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("remove pool %s: %w", p.Name, err)
		}
		c.Status.Pools = slices.DeleteFunc(c.Status.Pools, func(s nexov1.PoolStatus) bool { return s.Name == p.Name })
		r.event(c, corev1.EventTypeNormal, "PoolRemoved", "Remove", "pool %s removed (its PersistentVolumeClaims are kept)", p.Name)
		logger.Info("decommissioned pool removed", "pool", p.Name)
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels(selector(c))); err != nil {
		return ctrl.Result{}, err
	}
	st := observe(c, pods.Items, desired, shared)
	c.Status.ObservedGeneration = c.Generation
	c.Status.Ready = fmt.Sprintf("%d/%d", st.ready, st.total)
	var health *Health
	if st.ready > 0 {
		h, err := r.Health.Check(ctx, c, certs)
		if err != nil {
			logger.V(1).Info("health of the cluster unknown", "err", err)
		} else {
			health = &h
			c.Status.Health = &nexov1.HealthStatus{Nodes: h.Nodes, NodesOnline: h.NodesOnline, Drives: h.Drives, DrivesOnline: h.DrivesOnline,
				WriteQuorum: h.WriteQuorum, Maintenance: h.Maintenance, CheckedAt: metav1.Now()}
		}
	}

	if len(st.outdated) > 0 {
		r.setPhase(c, nexov1.PhaseUpdating)
		r.condition(c, nexov1.ConditionProgressing, true, "Updating", fmt.Sprintf("%d pod(s) to replace", len(st.outdated)))
		r.available(c, st, health)
		return r.rollout(ctx, c, st, health)
	}
	c.Status.Image = c.Spec.Image
	switch {
	case st.pods == 0:
		r.setPhase(c, nexov1.PhasePending)
		r.condition(c, nexov1.ConditionProgressing, true, "Creating", "waiting for the pods")
	case st.ready < st.total:
		phase := nexov1.PhaseStarting
		if c.Status.Phase == nexov1.PhaseReady || c.Status.Phase == nexov1.PhaseDegraded {
			phase = nexov1.PhaseDegraded
		}
		r.setPhase(c, phase)
		r.condition(c, nexov1.ConditionProgressing, phase == nexov1.PhaseStarting, "PodsNotReady", c.Status.Ready+" pods ready")
	case health != nil && !health.WriteQuorum:
		r.setPhase(c, nexov1.PhaseDegraded)
		r.condition(c, nexov1.ConditionProgressing, false, "Stable", "every pod is up to date")
	default:
		r.setPhase(c, nexov1.PhaseReady)
		r.condition(c, nexov1.ConditionProgressing, false, "Stable", "every pod is up to date")
	}
	r.available(c, st, health)
	if c.Status.Phase == nexov1.PhaseReady {
		return ctrl.Result{RequeueAfter: resyncRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: waitRequeue}, nil
}

// observed is the state of the pods of a cluster.
type observed struct {
	// total is the number of pods of the spec, pods the existing ones.
	total, pods, ready int
	// terminating pods are being deleted; outdated ones run another
	// template (sorted by name); sharedChanged ones another topology or
	// credentials.
	terminating   int
	outdated      []*corev1.Pod
	sharedChanged bool
}

// observe compares the pods with the templates of the pools.
func observe(c *nexov1.NexoCluster, pods []corev1.Pod, desired map[string]string, shared string) observed {
	var st observed
	ready, updated := map[string]int32{}, map[string]int32{}
	for _, p := range c.Spec.Pools {
		st.total += int(p.Servers)
	}
	slices.SortFunc(pods, func(a, b corev1.Pod) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	for i := range pods {
		pod := &pods[i]
		pool := pod.Labels[labelPool]
		want, ok := desired[pool]
		if !ok {
			continue
		}
		st.pods++
		if !pod.DeletionTimestamp.IsZero() {
			st.terminating++
			continue
		}
		if podReady(pod) {
			st.ready++
			ready[pool]++
		}
		if pod.Annotations[annotationTemplate] != want {
			st.outdated = append(st.outdated, pod)
			if pod.Annotations[annotationShared] != shared {
				st.sharedChanged = true
			}
		} else {
			updated[pool]++
		}
	}
	for i := range c.Status.Pools {
		c.Status.Pools[i].ReadyServers = ready[c.Status.Pools[i].Name]
		c.Status.Pools[i].UpdatedServers = updated[c.Status.Pools[i].Name]
	}
	return st
}

// rollout replaces the outdated pods: every one together for the
// AllAtOnce strategy or when what the nodes share changed (a topology, root
// credentials or KMS key the others do not have would not let a pod join);
// otherwise one at a time, once every pod is ready and the cluster can lose a
// node.
func (r *Reconciler) rollout(ctx context.Context, c *nexov1.NexoCluster, st observed, health *Health) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if c.Spec.UpdateStrategy == nexov1.UpdateAllAtOnce || st.sharedChanged {
		for _, pod := range st.outdated {
			if err := r.deletePod(ctx, c, pod); err != nil {
				return ctrl.Result{}, err
			}
		}
		logger.Info("replacing every outdated pod together", "pods", len(st.outdated), "sharedChanged", st.sharedChanged)
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}
	switch {
	case st.terminating > 0 || st.pods < st.total || st.ready < st.pods:
		// A pod is being replaced or is not ready yet.
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	case health == nil:
		r.condition(c, nexov1.ConditionProgressing, true, "WaitingForHealth", "the health of the cluster is unknown")
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	case !health.Stoppable():
		r.condition(c, nexov1.ConditionProgressing, true, "WaitingForQuorum",
			fmt.Sprintf("the cluster cannot lose a node yet (%d/%d nodes, %d/%d drives online)",
				health.NodesOnline, health.Nodes, health.DrivesOnline, health.Drives))
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}
	pod := st.outdated[0]
	if err := r.deletePod(ctx, c, pod); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("replacing an outdated pod", "pod", pod.Name, "left", len(st.outdated)-1)
	return ctrl.Result{RequeueAfter: waitRequeue}, nil
}

func (r *Reconciler) deletePod(ctx context.Context, c *nexov1.NexoCluster, pod *corev1.Pod) error {
	err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("replace pod %s: %w", pod.Name, err)
	}
	r.event(c, corev1.EventTypeNormal, "PodReplaced", "Replace", "pod %s replaced with the new template", pod.Name)
	return nil
}

// apply creates or updates an object owned by the cluster. The claims of an
// existing StatefulSet are immutable: only its template and replicas change.
func (r *Reconciler) apply(ctx context.Context, c *nexov1.NexoCluster, want client.Object) error {
	cur := want.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cur, func() error {
		switch o := cur.(type) {
		case *appsv1.StatefulSet:
			w := want.(*appsv1.StatefulSet)
			if o.CreationTimestamp.IsZero() {
				o.Spec = w.Spec
			} else {
				o.Spec.Replicas, o.Spec.Template, o.Spec.UpdateStrategy = w.Spec.Replicas, w.Spec.Template, w.Spec.UpdateStrategy
				o.Spec.PersistentVolumeClaimRetentionPolicy = w.Spec.PersistentVolumeClaimRetentionPolicy
			}
		case *corev1.Service:
			w := want.(*corev1.Service)
			// The cluster IP and the node ports are allocated by the API.
			o.Spec.Type, o.Spec.Selector, o.Spec.Ports = w.Spec.Type, w.Spec.Selector, mergePorts(o.Spec.Ports, w.Spec.Ports)
			o.Spec.PublishNotReadyAddresses = w.Spec.PublishNotReadyAddresses
			if o.CreationTimestamp.IsZero() {
				o.Spec.ClusterIP = w.Spec.ClusterIP
			}
		case *policyv1.PodDisruptionBudget:
			o.Spec = want.(*policyv1.PodDisruptionBudget).Spec
		}
		labels := cur.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range want.GetLabels() {
			labels[k] = v
		}
		cur.SetLabels(labels)
		return controllerutil.SetControllerReference(c, cur, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply %T %s: %w", want, want.GetName(), err)
	}
	return nil
}

// mergePorts keeps the node ports the API allocated.
func mergePorts(cur, want []corev1.ServicePort) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(want))
	for _, w := range want {
		for _, c := range cur {
			if c.Name == w.Name && c.Port == w.Port {
				w.NodePort = c.NodePort
			}
		}
		out = append(out, w)
	}
	return out
}

// secret returns a Secret of the cluster that must hold keys.
func (r *Reconciler) secret(ctx context.Context, c *nexov1.NexoCluster, name string, keys ...string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: Secret %s not found", errInvalid, name)
		}
		return nil, err
	}
	for _, k := range keys {
		if len(s.Data[k]) == 0 {
			return nil, fmt.Errorf("%w: Secret %s has no key %s", errInvalid, name, k)
		}
	}
	return s, nil
}

func (r *Reconciler) setPhase(c *nexov1.NexoCluster, phase nexov1.Phase) {
	if c.Status.Phase != phase {
		r.event(c, corev1.EventTypeNormal, "Phase", "Reconcile", "phase %s", phase)
	}
	c.Status.Phase = phase
	if phase != nexov1.PhaseInvalid {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: nexov1.ConditionDegraded,
			Status: boolStatus(phase == nexov1.PhaseDegraded), Reason: string(phase), ObservedGeneration: c.Generation})
	}
}

func (r *Reconciler) condition(c *nexov1.NexoCluster, typ string, status bool, reason, msg string) {
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: typ, Status: boolStatus(status), Reason: reason, Message: msg,
		ObservedGeneration: c.Generation})
}

// available sets the Available condition: every pod ready and the write
// quorum reported.
func (r *Reconciler) available(c *nexov1.NexoCluster, st observed, health *Health) {
	ok := st.total > 0 && st.ready == st.total && health != nil && health.WriteQuorum
	msg := c.Status.Ready + " pods ready"
	if health != nil {
		msg += fmt.Sprintf(", %d/%d drives online, write quorum %t", health.DrivesOnline, health.Drives, health.WriteQuorum)
	}
	r.condition(c, nexov1.ConditionAvailable, ok, map[bool]string{true: "Available", false: "Unavailable"}[ok], msg)
}

func (r *Reconciler) event(c *nexov1.NexoCluster, typ, reason, action, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(c, nil, typ, reason, action, format, args...)
	}
}

func boolStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
