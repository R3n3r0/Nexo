package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

// Health is the cluster health a node reports (/nexo/health/cluster).
type Health struct {
	Nodes        int  `json:"nodes"`
	NodesOnline  int  `json:"nodesOnline"`
	Drives       int  `json:"drives"`
	DrivesOnline int  `json:"drivesOnline"`
	WriteQuorum  bool `json:"writeQuorum"`
	// Maintenance reports whether one more node can stop without losing
	// the write quorum of any erasure set.
	Maintenance bool `json:"maintenance"`
}

// Stoppable reports whether a node of the cluster may be restarted now.
func (h Health) Stoppable() bool {
	return h.WriteQuorum && h.Maintenance && h.Nodes > 0 && h.NodesOnline == h.Nodes && h.DrivesOnline == h.Drives
}

// HealthChecker reads the health of a cluster.
type HealthChecker interface {
	Check(ctx context.Context, c *nexov1.NexoCluster, tlsSecret *corev1.Secret) (Health, error)
}

// HTTPHealth reads /nexo/health/cluster through the S3 Service of the
// cluster. With TLS the server certificate is verified with the CA of the
// TLS Secret (ca.crt, else the certificate itself) and the system roots.
type HTTPHealth struct {
	Timeout time.Duration
	// DialContext opens the connections (tests; default: net.Dialer).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

var _ HealthChecker = HTTPHealth{}

// Check implements HealthChecker.
func (h HTTPHealth) Check(ctx context.Context, c *nexov1.NexoCluster, tlsSecret *corev1.Secret) (Health, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	host := fmt.Sprintf("%s.%s.svc.%s", c.Name, c.Namespace, clusterDomain(c))
	transport := &http.Transport{DialContext: h.DialContext}
	if c.Spec.TLS != nil {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if tlsSecret != nil {
			if ca := tlsSecret.Data[caKey]; len(ca) > 0 {
				roots.AppendCertsFromPEM(ca)
			} else {
				roots.AppendCertsFromPEM(tlsSecret.Data[corev1.TLSCertKey])
			}
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	defer client.CloseIdleConnections()
	url := fmt.Sprintf("%s://%s:%d/nexo/health/cluster", scheme(c), host, portS3)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Health{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 503 carries the same report: the cluster is unhealthy.
	var out Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Health{}, fmt.Errorf("health report (%s): %w", resp.Status, err)
	}
	return out, nil
}
