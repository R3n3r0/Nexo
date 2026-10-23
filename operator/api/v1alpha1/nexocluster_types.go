package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpdateStrategy is how the pods take a new template (a new image, new
// settings).
// +kubebuilder:validation:Enum=Rolling;AllAtOnce
type UpdateStrategy string

const (
	// UpdateRolling replaces one pod at a time, once the deployment can lose
	// a node without losing the write quorum of any erasure set.
	UpdateRolling UpdateStrategy = "Rolling"
	// UpdateAllAtOnce replaces every pod together (a short outage).
	UpdateAllAtOnce UpdateStrategy = "AllAtOnce"
)

// NexoClusterSpec is the desired deployment.
// +kubebuilder:validation:XValidation:rule="oldSelf.pools.all(p, self.pools.all(q, q.name != p.name || (q.servers == p.servers && q.volumesPerServer == p.volumesPerServer)))",message="a pool is never resized: add a new pool to expand"
type NexoClusterSpec struct {
	// Image is the container image of the server, e.g.
	// ghcr.io/r3n3r0/nexo:v1.0.0. Changing it updates the pods (see
	// updateStrategy).
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Credentials is the Secret of the root credentials (required, the same
	// for every node).
	Credentials CredentialsSpec `json:"credentials"`
	// KMS is the Secret of the master key of the builtin KMS
	// (NEXO_KMS_SECRET_KEY, "name:base64").
	// +optional
	KMS *KMSSpec `json:"kms,omitempty"`
	// TLS serves S3, the console and the internode traffic over TLS with
	// the certificate of a Secret (tls.crt, tls.key, optional ca.crt):
	// valid for the host names of the pods
	// (*.<name>-hl.<namespace>.svc.<clusterDomain>) and of the Services.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// Pools are the server pools, in order. A pool is never resized once
	// created: the cluster grows by appending a pool (every pod restarts with
	// the new topology). A pool is removed only once decommissioned
	// (nexoctl admin decommission) and named in the annotation
	// nexo.r3n3r0.github.io/decommissioned-pools.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Pools []PoolSpec `json:"pools"`

	// UpdateStrategy is how the pods take a new template; a change of the
	// pools always restarts every pod together. Default Rolling.
	// +kubebuilder:default=Rolling
	// +optional
	UpdateStrategy UpdateStrategy `json:"updateStrategy,omitempty"`

	// Env adds environment variables to the server (NEXO_* settings).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Resources of the server container, unless a pool sets its own.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// ServiceType of the S3 and console Services (default ClusterIP).
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// ClusterDomain is the DNS domain of the cluster (default
	// cluster.local).
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// ServiceAccountName runs the pods (default: the default account).
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// PodLabels and PodAnnotations are added to the pods.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// PriorityClassName of the pods.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// CredentialsSpec names the Secret of the root credentials.
type CredentialsSpec struct {
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
	// UserKey and PasswordKey are the keys of the Secret (default
	// root-user and root-password).
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// KMSSpec names the Secret of the master key of the builtin KMS.
type KMSSpec struct {
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
	// Key of the Secret (default kms-secret-key).
	// +optional
	Key string `json:"key,omitempty"`
}

// TLSSpec names the Secret of the certificate of the servers.
type TLSSpec struct {
	// SecretName is a kubernetes.io/tls Secret, with an optional ca.crt.
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
}

// PoolSpec is a pool of servers.
type PoolSpec struct {
	// Name of the pool: the StatefulSet is <cluster>-<name>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=20
	Name string `json:"name"`
	// Servers is the number of pods of the pool.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	Servers int32 `json:"servers"`
	// VolumesPerServer is the number of drives (PersistentVolumeClaims) of
	// each pod.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	VolumesPerServer int32 `json:"volumesPerServer"`
	// Storage is the size of each drive.
	Storage resource.Quantity `json:"storage"`
	// StorageClassName of the drives (default: the default class).
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// Resources of the server container of the pool.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity of the pods (default: spread the pods of the pool on
	// distinct nodes when possible).
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// Phase is the summary state of a cluster.
type Phase string

const (
	PhasePending  Phase = "Pending"
	PhaseStarting Phase = "Starting"
	PhaseReady    Phase = "Ready"
	PhaseUpdating Phase = "Updating"
	PhaseDegraded Phase = "Degraded"
	PhaseInvalid  Phase = "Invalid"
)

// Condition types.
const (
	// ConditionAvailable: every pod ready and the cluster reports its
	// write quorum.
	ConditionAvailable = "Available"
	// ConditionProgressing: pods are being created or updated.
	ConditionProgressing = "Progressing"
	// ConditionDegraded: the spec cannot be applied, or the cluster lost
	// pods or drives.
	ConditionDegraded = "Degraded"
)

// NexoClusterStatus is the observed state.
type NexoClusterStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Ready is "<ready pods>/<pods>".
	// +optional
	Ready string `json:"ready,omitempty"`
	// Image every pod runs, once no update is in progress.
	// +optional
	Image string `json:"image,omitempty"`
	// Pools are the pools created, with their topology.
	// +optional
	Pools []PoolStatus `json:"pools,omitempty"`
	// Health is the last report of /nexo/health/cluster.
	// +optional
	Health *HealthStatus `json:"health,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PoolStatus is a pool created.
type PoolStatus struct {
	Name             string `json:"name"`
	Servers          int32  `json:"servers"`
	VolumesPerServer int32  `json:"volumesPerServer"`
	ReadyServers     int32  `json:"readyServers"`
	UpdatedServers   int32  `json:"updatedServers"`
}

// HealthStatus is a report of /nexo/health/cluster.
type HealthStatus struct {
	Nodes        int  `json:"nodes"`
	NodesOnline  int  `json:"nodesOnline"`
	Drives       int  `json:"drives"`
	DrivesOnline int  `json:"drivesOnline"`
	WriteQuorum  bool `json:"writeQuorum"`
	// Maintenance reports whether one more node may go down.
	Maintenance bool        `json:"maintenance"`
	CheckedAt   metav1.Time `json:"checkedAt"`
}

// NexoCluster is a Nexo deployment: one StatefulSet per pool, a headless
// Service for the host names of the pods, the S3 and console Services and a
// PodDisruptionBudget. The PersistentVolumeClaims of the drives are never
// deleted by the operator, not even with the cluster.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=nexo,categories=all
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NexoCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NexoClusterSpec   `json:"spec"`
	Status NexoClusterStatus `json:"status,omitempty"`
}

// NexoClusterList is a list of clusters.
// +kubebuilder:object:root=true
type NexoClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NexoCluster `json:"items"`
}
