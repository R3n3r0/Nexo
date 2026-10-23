package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

func testCluster(pools ...nexov1.PoolSpec) *nexov1.NexoCluster {
	if len(pools) == 0 {
		pools = []nexov1.PoolSpec{{Name: "p0", Servers: 4, VolumesPerServer: 2, Storage: resource.MustParse("10Gi")}}
	}
	return &nexov1.NexoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lake", Namespace: "storage"},
		Spec:       nexov1.NexoClusterSpec{Image: "ghcr.io/r3n3r0/nexo:v1.0.0", Credentials: nexov1.CredentialsSpec{SecretName: "creds"}, Pools: pools},
	}
}

// TestEndpoints: one argument per pool, in order, naming the pods of its
// StatefulSet through the headless Service, https with TLS.
//
// covers: S-OPS-16
func TestEndpoints(t *testing.T) {
	c := testCluster(
		nexov1.PoolSpec{Name: "p0", Servers: 4, VolumesPerServer: 2},
		nexov1.PoolSpec{Name: "big", Servers: 1, VolumesPerServer: 1},
	)
	assert.Equal(t, []string{
		"http://lake-p0-{0...3}.lake-hl.storage.svc.cluster.local:9000/data{1...2}",
		"http://lake-big-0.lake-hl.storage.svc.cluster.local:9000/data1",
	}, endpoints(c))
	c.Spec.TLS = &nexov1.TLSSpec{SecretName: "certs"}
	c.Spec.ClusterDomain = "k8s.example"
	assert.True(t, strings.HasPrefix(endpoints(c)[0], "https://lake-p0-{0...3}.lake-hl.storage.svc.k8s.example:9000/"))
}

// TestStatefulSet: the operator replaces the pods (OnDelete), the claims of
// the drives are kept, the pods start together and mount one claim per drive
// and the secrets.
//
// covers: S-OPS-16
func TestStatefulSet(t *testing.T) {
	c := testCluster()
	c.Spec.KMS = &nexov1.KMSSpec{SecretName: "kms"}
	c.Spec.TLS = &nexov1.TLSSpec{SecretName: "certs"}
	sts := statefulSet(c, c.Spec.Pools[0], "shared")
	assert.Equal(t, "lake-p0", sts.Name)
	assert.Equal(t, int32(4), *sts.Spec.Replicas)
	assert.Equal(t, "lake-hl", sts.Spec.ServiceName)
	assert.Equal(t, appsv1.OnDeleteStatefulSetStrategyType, sts.Spec.UpdateStrategy.Type)
	assert.Equal(t, appsv1.ParallelPodManagement, sts.Spec.PodManagementPolicy)
	assert.Equal(t, appsv1.RetainPersistentVolumeClaimRetentionPolicyType, sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted)
	assert.Equal(t, appsv1.RetainPersistentVolumeClaimRetentionPolicyType, sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled)
	require.Len(t, sts.Spec.VolumeClaimTemplates, 2)
	assert.Equal(t, "data1", sts.Spec.VolumeClaimTemplates[0].Name)
	ct := sts.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/r3n3r0/nexo:v1.0.0", ct.Image)
	assert.Equal(t, "server", ct.Args[0])
	mounts := map[string]string{}
	for _, m := range ct.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	assert.Equal(t, "/data1", mounts["data1"])
	assert.Equal(t, "/data2", mounts["data2"])
	assert.Equal(t, "/etc/nexo/certs", mounts["certs"])
	env := map[string]string{}
	for _, e := range ct.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "/run/secrets/nexo/kms-secret-key", env["NEXO_KMS_SECRET_KEY_FILE"])
	assert.Equal(t, "/etc/nexo/certs", env["NEXO_CERTS_DIR"])
	assert.Equal(t, corev1.URISchemeHTTPS, ct.ReadinessProbe.HTTPGet.Scheme)
	assert.True(t, *ct.SecurityContext.ReadOnlyRootFilesystem)
	assert.Equal(t, "p0", sts.Spec.Template.Labels[labelPool])
	assert.Equal(t, "shared", sts.Spec.Template.Annotations[annotationShared])
	assert.NotEmpty(t, sts.Spec.Template.Annotations[annotationTemplate])
}

// TestHashes: a new image changes the template of the pods but not what the
// nodes share; a new pool or new root credentials change what they share.
//
// covers: S-OPS-16
func TestHashes(t *testing.T) {
	c := testCluster()
	creds := &corev1.Secret{Data: map[string][]byte{"root-user": []byte("admin"), "root-password": []byte("secret")}}
	shared := sharedHash(c, creds)
	tpl := podTemplate(c, c.Spec.Pools[0], shared).Annotations[annotationTemplate]
	assert.Equal(t, tpl, podTemplate(c, c.Spec.Pools[0], shared).Annotations[annotationTemplate], "stable")

	c.Spec.Image = "ghcr.io/r3n3r0/nexo:v1.1.0"
	assert.Equal(t, shared, sharedHash(c, creds))
	assert.NotEqual(t, tpl, podTemplate(c, c.Spec.Pools[0], shared).Annotations[annotationTemplate])

	c.Spec.Pools = append(c.Spec.Pools, nexov1.PoolSpec{Name: "p1", Servers: 4, VolumesPerServer: 2})
	assert.NotEqual(t, shared, sharedHash(c, creds), "a new topology")
	c.Spec.Pools = c.Spec.Pools[:1]
	rotated := &corev1.Secret{Data: map[string][]byte{"root-user": []byte("admin"), "root-password": []byte("other")}}
	assert.NotEqual(t, shared, sharedHash(c, rotated), "new root credentials")
}

// TestValidate: the pools created keep their order and size; new pools are
// appended; a pool is removed only when confirmed decommissioned.
//
// covers: S-OPS-16
func TestValidate(t *testing.T) {
	p0 := nexov1.PoolSpec{Name: "p0", Servers: 4, VolumesPerServer: 2}
	p1 := nexov1.PoolSpec{Name: "p1", Servers: 4, VolumesPerServer: 4}
	created := []nexov1.PoolStatus{{Name: "p0", Servers: 4, VolumesPerServer: 2}, {Name: "p1", Servers: 4, VolumesPerServer: 4}}
	tests := []struct {
		name        string
		pools       []nexov1.PoolSpec
		status      []nexov1.PoolStatus
		annotation  string
		wantErr     string
		wantRemoved []string
	}{
		{name: "new cluster", pools: []nexov1.PoolSpec{p0, p1}},
		{name: "unchanged", pools: []nexov1.PoolSpec{p0, p1}, status: created},
		{name: "appended", pools: []nexov1.PoolSpec{p0, p1, {Name: "p2", Servers: 2, VolumesPerServer: 2}}, status: created},
		{name: "inserted", pools: []nexov1.PoolSpec{{Name: "p2", Servers: 2, VolumesPerServer: 2}, p0, p1}, status: created, wantErr: "appended"},
		{name: "reordered", pools: []nexov1.PoolSpec{p1, p0}, status: created, wantErr: "order"},
		{name: "resized", pools: []nexov1.PoolSpec{p0, {Name: "p1", Servers: 8, VolumesPerServer: 4}}, status: created, wantErr: "never resized"},
		{name: "duplicate", pools: []nexov1.PoolSpec{p0, p0}, wantErr: "twice"},
		{name: "removed", pools: []nexov1.PoolSpec{p1}, status: created, wantErr: "decommission it first"},
		{name: "removed once decommissioned", pools: []nexov1.PoolSpec{p1}, status: created, annotation: "p0", wantRemoved: []string{"p0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testCluster(tt.pools...)
			c.Status.Pools = tt.status
			if tt.annotation != "" {
				c.Annotations = map[string]string{annotationDecommissioned: tt.annotation}
			}
			removed, err := validate(c)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			var names []string
			for _, p := range removed {
				names = append(names, p.Name)
			}
			assert.Equal(t, tt.wantRemoved, names)
		})
	}
}

// TestHealthStoppable: a node may be restarted only while every node and
// drive is online and the cluster keeps its write quorum without one more
// node.
//
// covers: S-OPS-16
func TestHealthStoppable(t *testing.T) {
	ok := Health{Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 8, WriteQuorum: true, Maintenance: true}
	assert.True(t, ok.Stoppable())
	for _, h := range []Health{
		{Nodes: 4, NodesOnline: 3, Drives: 8, DrivesOnline: 6, WriteQuorum: true, Maintenance: true},
		{Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 7, WriteQuorum: true, Maintenance: true},
		{Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 8, WriteQuorum: true},
		{},
	} {
		assert.False(t, h.Stoppable(), "%+v", h)
	}
}
