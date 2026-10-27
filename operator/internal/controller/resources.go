package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

// Labels and annotations of the objects of a cluster.
const (
	labelCluster = "nexo.r3n3r0.github.io/cluster"
	labelPool    = "nexo.r3n3r0.github.io/pool"
	// annotationTemplate is the hash of the pod template of a pool: a pod
	// whose hash differs runs an outdated template.
	annotationTemplate = "nexo.r3n3r0.github.io/template-hash"
	// annotationShared is the hash of what every node must share (the
	// topology, the root credentials, the KMS key): pods that differ cannot
	// form a cluster with the others, so they are all replaced together.
	annotationShared = "nexo.r3n3r0.github.io/shared-hash"
)

// Ports of the server.
const (
	portS3      = 9000
	portConsole = 9001
)

// Defaults of the spec.
const (
	defaultUserKey       = "root-user"
	defaultPasswordKey   = "root-password"
	defaultKMSKey        = "kms-secret-key"
	defaultClusterDomain = "cluster.local"
	// caKey is the key of the CA in a TLS Secret (cert-manager).
	caKey = "ca.crt"
)

func headlessName(c *nexov1.NexoCluster) string { return c.Name + "-hl" }
func consoleName(c *nexov1.NexoCluster) string  { return c.Name + "-console" }
func poolName(c *nexov1.NexoCluster, p nexov1.PoolSpec) string {
	return c.Name + "-" + p.Name
}

func clusterDomain(c *nexov1.NexoCluster) string {
	if c.Spec.ClusterDomain != "" {
		return c.Spec.ClusterDomain
	}
	return defaultClusterDomain
}

func scheme(c *nexov1.NexoCluster) string {
	if c.Spec.TLS != nil {
		return "https"
	}
	return "http"
}

// selector selects every pod of the cluster.
func selector(c *nexov1.NexoCluster) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "nexo", labelCluster: c.Name}
}

// poolSelector selects the pods of a pool.
func poolSelector(c *nexov1.NexoCluster, p nexov1.PoolSpec) map[string]string {
	s := selector(c)
	s[labelPool] = p.Name
	return s
}

func labels(c *nexov1.NexoCluster) map[string]string {
	l := selector(c)
	l["app.kubernetes.io/instance"] = c.Name
	l["app.kubernetes.io/managed-by"] = "nexo-operator"
	return l
}

// endpoints returns the drive arguments of the server: one per pool, in
// order, every node running the same command line (docs/08):
//
//	http://<cluster>-<pool>-{0...N-1}.<cluster>-hl.<ns>.svc.<domain>:9000/data{1...V}
func endpoints(c *nexov1.NexoCluster) []string {
	out := make([]string, 0, len(c.Spec.Pools))
	domain := fmt.Sprintf("%s.%s.svc.%s", headlessName(c), c.Namespace, clusterDomain(c))
	for _, p := range c.Spec.Pools {
		host := fmt.Sprintf("%s-{0...%d}.%s", poolName(c, p), p.Servers-1, domain)
		if p.Servers == 1 {
			host = fmt.Sprintf("%s-0.%s", poolName(c, p), domain)
		}
		out = append(out, fmt.Sprintf("%s://%s:%d%s", scheme(c), host, portS3, drivePaths(p)))
	}
	return out
}

func drivePaths(p nexov1.PoolSpec) string {
	if p.VolumesPerServer == 1 {
		return "/data1"
	}
	return fmt.Sprintf("/data{1...%d}", p.VolumesPerServer)
}

// hashOf is a short stable digest of v.
func hashOf(v ...any) string {
	h := sha256.New()
	for _, x := range v {
		data, err := json.Marshal(x)
		if err != nil {
			panic(err) // the values are API types: always encodable
		}
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sharedHash is the digest of what every node must share: the topology and
// the data of the root credentials and of the KMS key.
func sharedHash(c *nexov1.NexoCluster, secrets ...*corev1.Secret) string {
	data := make([]map[string][]byte, 0, len(secrets))
	for _, s := range secrets {
		if s != nil {
			data = append(data, s.Data)
		}
	}
	return hashOf(endpoints(c), data)
}

// podTemplate returns the pod template of a pool, its hashes in its
// annotations.
func podTemplate(c *nexov1.NexoCluster, p nexov1.PoolSpec, shared string) corev1.PodTemplateSpec {
	spec := c.Spec
	env := []corev1.EnvVar{
		{Name: "NEXO_ADDRESS", Value: fmt.Sprintf(":%d", portS3)},
		{Name: "NEXO_CONSOLE_ADDRESS", Value: fmt.Sprintf(":%d", portConsole)},
		{Name: "NEXO_ROOT_USER_FILE", Value: "/run/secrets/nexo/root-user"},
		{Name: "NEXO_ROOT_PASSWORD_FILE", Value: "/run/secrets/nexo/root-password"},
	}
	userKey, passKey := spec.Credentials.UserKey, spec.Credentials.PasswordKey
	if userKey == "" {
		userKey = defaultUserKey
	}
	if passKey == "" {
		passKey = defaultPasswordKey
	}
	secretSources := []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{
		LocalObjectReference: corev1.LocalObjectReference{Name: spec.Credentials.SecretName},
		Items:                []corev1.KeyToPath{{Key: userKey, Path: "root-user"}, {Key: passKey, Path: "root-password"}},
	}}}
	if spec.KMS != nil {
		key := spec.KMS.Key
		if key == "" {
			key = defaultKMSKey
		}
		env = append(env, corev1.EnvVar{Name: "NEXO_KMS_SECRET_KEY_FILE", Value: "/run/secrets/nexo/kms-secret-key"})
		secretSources = append(secretSources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: spec.KMS.SecretName},
			Items:                []corev1.KeyToPath{{Key: key, Path: "kms-secret-key"}},
		}})
	}
	volumes := []corev1.Volume{
		{Name: "secrets", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o440), Sources: secretSources}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := make([]corev1.VolumeMount, 0, p.VolumesPerServer+3)
	for i := 1; i <= int(p.VolumesPerServer); i++ {
		mounts = append(mounts, corev1.VolumeMount{Name: fmt.Sprintf("data%d", i), MountPath: fmt.Sprintf("/data%d", i)})
	}
	mounts = append(mounts,
		corev1.VolumeMount{Name: "secrets", MountPath: "/run/secrets/nexo", ReadOnly: true},
		corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
	probeScheme := corev1.URISchemeHTTP
	if spec.TLS != nil {
		probeScheme = corev1.URISchemeHTTPS
		env = append(env, corev1.EnvVar{Name: "NEXO_CERTS_DIR", Value: "/etc/nexo/certs"})
		// The certificates are reloaded by the server when the Secret
		// changes: they are not part of the template hash.
		volumes = append(volumes, corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o440),
			Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.TLS.SecretName},
					Items:                []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: "public.crt"}, {Key: corev1.TLSPrivateKeyKey, Path: "private.key"}},
				}},
				// The CA of a private PKI, when the Secret has one.
				{Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.TLS.SecretName},
					Items:                []corev1.KeyToPath{{Key: caKey, Path: "CAs/ca.crt"}},
					Optional:             ptr.To(true),
				}},
			},
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "certs", MountPath: "/etc/nexo/certs", ReadOnly: true})
	}
	env = append(env, spec.Env...)
	resources := spec.Resources
	if p.Resources != nil {
		resources = *p.Resources
	}
	probe := func(path string, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("s3"), Scheme: probeScheme}},
			PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: failures,
		}
	}
	affinity := p.Affinity
	if affinity == nil {
		affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{
				TopologyKey:   corev1.LabelHostname,
				LabelSelector: &metav1.LabelSelector{MatchLabels: poolSelector(c, p)},
			}}},
		}}
	}
	podLabels := maps.Clone(spec.PodLabels)
	if podLabels == nil {
		podLabels = map[string]string{}
	}
	maps.Copy(podLabels, poolSelector(c, p))
	podLabels["app.kubernetes.io/managed-by"] = "nexo-operator"
	tpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: maps.Clone(spec.PodAnnotations)},
		Spec: corev1.PodSpec{
			ServiceAccountName:            spec.ServiceAccountName,
			ImagePullSecrets:              spec.ImagePullSecrets,
			PriorityClassName:             spec.PriorityClassName,
			TerminationGracePeriodSeconds: ptr.To[int64](60),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532), FSGroup: ptr.To[int64](65532),
				FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			NodeSelector: p.NodeSelector,
			Tolerations:  p.Tolerations,
			Affinity:     affinity,
			Containers: []corev1.Container{{
				Name:            "nexo",
				Image:           spec.Image,
				ImagePullPolicy: spec.ImagePullPolicy,
				Args:            append([]string{"server"}, endpoints(c)...),
				Env:             env,
				Ports: []corev1.ContainerPort{
					{Name: "s3", ContainerPort: portS3, Protocol: corev1.ProtocolTCP},
					{Name: "console", ContainerPort: portConsole, Protocol: corev1.ProtocolTCP},
				},
				// The bootstrap of a cluster waits for a quorum of the
				// nodes: up to 10 minutes.
				StartupProbe:   probe("/nexo/health/live", 60),
				LivenessProbe:  probe("/nexo/health/live", 3),
				ReadinessProbe: probe("/nexo/health/ready", 3),
				Resources:      resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Privileged: ptr.To(false),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[annotationShared] = shared
	tpl.Annotations[annotationTemplate] = hashOf(tpl.Spec, tpl.Labels, tpl.Annotations)
	return tpl
}

// statefulSet returns the StatefulSet of a pool. Its pods are replaced by
// the operator (OnDelete), never by the StatefulSet controller; the claims
// of the drives are kept when the StatefulSet is deleted or scaled.
func statefulSet(c *nexov1.NexoCluster, p nexov1.PoolSpec, shared string) *appsv1.StatefulSet {
	claims := make([]corev1.PersistentVolumeClaim, 0, p.VolumesPerServer)
	for i := 1; i <= int(p.VolumesPerServer); i++ {
		claims = append(claims, corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("data%d", i), Labels: poolSelector(c, p)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: p.StorageClassName,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: p.Storage}},
			},
		})
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: poolName(c, p), Namespace: c.Namespace, Labels: labels(c)},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(p.Servers),
			ServiceName:         headlessName(c),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Selector:             &metav1.LabelSelector{MatchLabels: poolSelector(c, p)},
			Template:             podTemplate(c, p, shared),
			VolumeClaimTemplates: claims,
		},
	}
}

// headlessService gives the pods their stable host names; the nodes reach
// each other before they are ready (the bootstrap).
func headlessService(c *nexov1.NexoCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: headlessName(c), Namespace: c.Namespace, Labels: labels(c)},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 selector(c),
			Ports:                    []corev1.ServicePort{{Name: "s3", Port: portS3, TargetPort: intstr.FromString("s3")}},
		},
	}
}

// service returns the S3 Service (name) or the console one.
func service(c *nexov1.NexoCluster, name, port string, number int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels(c)},
		Spec: corev1.ServiceSpec{
			Type:     c.Spec.ServiceType,
			Selector: selector(c),
			Ports:    []corev1.ServicePort{{Name: port, Port: number, TargetPort: intstr.FromString(port)}},
		},
	}
}

// disruptionBudget lets the voluntary disruptions (node drains) take one
// pod of the cluster at a time.
func disruptionBudget(c *nexov1.NexoCluster) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, Labels: labels(c)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: selector(c)},
		},
	}
}

// annotationDecommissioned lists the pools (comma separated) the user
// confirms decommissioned: only those may be removed from the spec.
const annotationDecommissioned = "nexo.r3n3r0.github.io/decommissioned-pools"

// validate checks what the API cannot: the pools created (status.pools)
// keep their order, new pools are appended, and a pool is removed only when
// the user confirms it decommissioned. It returns the pools to remove.
func validate(c *nexov1.NexoCluster) ([]nexov1.PoolStatus, error) {
	spec := map[string]nexov1.PoolSpec{}
	for _, p := range c.Spec.Pools {
		if _, dup := spec[p.Name]; dup {
			return nil, fmt.Errorf("pool %q is declared twice", p.Name)
		}
		spec[p.Name] = p
	}
	confirmed := map[string]bool{}
	for _, n := range strings.Split(c.Annotations[annotationDecommissioned], ",") {
		if n = strings.TrimSpace(n); n != "" {
			confirmed[n] = true
		}
	}
	var removed []nexov1.PoolStatus
	next := 0 // index in the spec of the next created pool
	for _, created := range c.Status.Pools {
		p, ok := spec[created.Name]
		if !ok {
			if !confirmed[created.Name] {
				return nil, fmt.Errorf("pool %q was removed: decommission it first (nexoctl admin decommission), then confirm it in the annotation %s",
					created.Name, annotationDecommissioned)
			}
			removed = append(removed, created)
			continue
		}
		if p.Servers != created.Servers || p.VolumesPerServer != created.VolumesPerServer {
			return nil, fmt.Errorf("pool %q was created with %d servers of %d volumes: a pool is never resized, add a new pool",
				p.Name, created.Servers, created.VolumesPerServer)
		}
		i := slices.IndexFunc(c.Spec.Pools, func(q nexov1.PoolSpec) bool { return q.Name == created.Name })
		if i < next {
			return nil, fmt.Errorf("pool %q moved: the pools keep their order, new pools are appended", p.Name)
		}
		next = i + 1
	}
	// The new pools come after the existing ones.
	for _, q := range c.Spec.Pools[:next] {
		if !slices.ContainsFunc(c.Status.Pools, func(s nexov1.PoolStatus) bool { return s.Name == q.Name }) {
			return nil, fmt.Errorf("pool %q is new: new pools are appended after the existing ones", q.Name)
		}
	}
	return removed, nil
}
