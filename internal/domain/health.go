package domain

// Health probe paths. They are served by both the S3 and the console
// listeners and are part of the public contract of the server (Docker
// HEALTHCHECK, Kubernetes probes, load balancers), so they live here where
// both the transports and the commands can reach them without crossing a
// layer.
const (
	HealthLivePath    = "/nexo/health/live"
	HealthReadyPath   = "/nexo/health/ready"
	HealthClusterPath = "/nexo/health/cluster"
)
