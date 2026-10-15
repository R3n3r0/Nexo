package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The metrics endpoints of a Nexo server (docs/07 §7.4), served on its S3
// listener under PrometheusMetricsPath.
const (
	PrometheusCluster = "cluster"
	PrometheusNode    = "node"
	PrometheusBucket  = "bucket"
)

// PrometheusMetricsPath is the path prefix of the metrics endpoints.
const PrometheusMetricsPath = "/nexo/metrics/v1/"

// PrometheusTokenIssuer is the issuer claim of the bearer tokens of the
// metrics endpoints (NEXO_PROMETHEUS_AUTH_TYPE=jwt, docs/07 §7.6).
const PrometheusTokenIssuer = "prometheus"

// SignPrometheusToken returns a bearer token of the metrics endpoints for
// an access key: a JWT (HS512) signed with its secret key, whose subject
// is the access key and issuer PrometheusTokenIssuer, issued at now and
// expiring at exp. The server accepts it while the access key exists with
// that secret key and admin:Prometheus.
func SignPrometheusToken(accessKey, secretKey string, now, exp time.Time) (string, error) {
	if accessKey == "" || secretKey == "" {
		return "", UsageError("the alias has no credentials to sign the token with")
	}
	if !exp.After(now) {
		return "", UsageError("the token must expire in the future")
	}
	claims := jwt.RegisteredClaims{
		Subject:   accessKey,
		Issuer:    PrometheusTokenIssuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(secretKey))
	if err != nil {
		return "", fmt.Errorf("sign the token: %w", err)
	}
	return token, nil
}

// PrometheusScrape is the scrape configuration of a metrics endpoint.
type PrometheusScrape struct {
	JobName     string `json:"job"`
	Scheme      string `json:"scheme"`
	Target      string `json:"target"`
	MetricsPath string `json:"metricsPath"`
	// Interval is the scrape interval of the job (0: the global one).
	Interval time.Duration `json:"-"`
	// BearerToken authenticates the scrapes ("": a public endpoint).
	BearerToken string `json:"-"`
}

// NewPrometheusScrape returns the scrape configuration of the metrics of
// kind (PrometheusCluster, PrometheusNode or PrometheusBucket) of the
// server whose S3 endpoint is endpoint; job "" names it nexo-<kind>. The
// bucket metrics are scraped every minute (they are costlier).
func NewPrometheusScrape(endpoint, kind, job, token string) (PrometheusScrape, error) {
	switch kind {
	case PrometheusCluster, PrometheusNode, PrometheusBucket:
	default:
		return PrometheusScrape{}, UsageError("metrics %q must be cluster, node or bucket", kind)
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return PrometheusScrape{}, UsageError("the URL %q of the alias is not an http(s) URL", endpoint)
	}
	if job == "" {
		job = "nexo-" + kind
	}
	s := PrometheusScrape{
		JobName:     job,
		Scheme:      u.Scheme,
		Target:      u.Host,
		MetricsPath: strings.TrimSuffix(u.Path, "/") + PrometheusMetricsPath + kind,
		BearerToken: token,
	}
	if kind == PrometheusBucket {
		s.Interval = time.Minute
	}
	return s, nil
}

// YAML renders the scrape configuration as a scrape_configs section of
// prometheus.yml. Strings are quoted (JSON strings are YAML scalars).
func (s PrometheusScrape) YAML() string {
	q := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	var b strings.Builder
	b.WriteString("scrape_configs:\n")
	fmt.Fprintf(&b, "  - job_name: %s\n", q(s.JobName))
	fmt.Fprintf(&b, "    metrics_path: %s\n", q(s.MetricsPath))
	fmt.Fprintf(&b, "    scheme: %s\n", s.Scheme)
	if s.Interval > 0 {
		fmt.Fprintf(&b, "    scrape_interval: %s\n", promDuration(s.Interval))
	}
	if s.BearerToken != "" {
		b.WriteString("    authorization:\n      type: Bearer\n")
		fmt.Fprintf(&b, "      credentials: %s\n", q(s.BearerToken))
	}
	b.WriteString("    static_configs:\n")
	fmt.Fprintf(&b, "      - targets: [%s]\n", q(s.Target))
	return b.String()
}

// promDuration renders a duration in the format of Prometheus (1m, 30s).
func promDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return fmt.Sprintf("%dms", d/time.Millisecond)
	}
}
