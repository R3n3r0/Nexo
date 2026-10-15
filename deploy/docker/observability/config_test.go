// Package observability_test validates the observability configuration
// shipped with the Docker deployment: the Prometheus scrape configurations,
// the alerting rules and the Grafana dashboard parse, and every Nexo metric
// they name exists (docs/05 T-8.1).
package observability_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// family is a metric family Nexo exposes; live marks the families a
// single node with a KMS exposes once it served a few requests (the
// others need a cluster, an erasure pool, a healing, a notification
// target or a distribution).
type family struct {
	histogram bool
	live      bool
}

// families are the Nexo families the deployment files may name (docs/07
// §7.4). TestFamiliesExposed checks the live ones against a running server.
func families() map[string]family {
	g := family{}
	live := family{live: true}
	h := family{histogram: true}
	liveH := family{histogram: true, live: true}
	return map[string]family{
		// S3 API (node).
		"nexo_s3_requests_total": live, "nexo_s3_requests_4xx_errors_total": g, "nexo_s3_requests_5xx_errors_total": g,
		"nexo_s3_request_duration_seconds": liveH, "nexo_s3_requests_inflight": live,
		"nexo_s3_traffic_received_bytes_total": live, "nexo_s3_traffic_sent_bytes_total": live,
		// Buckets (bucket endpoint).
		"nexo_bucket_requests_total": live, "nexo_bucket_traffic_received_bytes_total": live, "nexo_bucket_traffic_sent_bytes_total": live,
		"nexo_bucket_usage_total_bytes": g, "nexo_bucket_objects": g, "nexo_bucket_versions": g, "nexo_bucket_delete_markers": g,
		"nexo_bucket_usage_last_update_timestamp_seconds": g,
		// Drives (node).
		"nexo_drive_online": live, "nexo_drive_healing": live, "nexo_drive_total_bytes": live, "nexo_drive_used_bytes": live,
		"nexo_drive_free_bytes": live, "nexo_drive_used_inodes": live, "nexo_drive_free_inodes": live,
		"nexo_drive_operations_total": live, "nexo_drive_operation_seconds_total": live, "nexo_drive_errors_total": g,
		// Cluster (cluster endpoint).
		"nexo_cluster_nodes_online": live, "nexo_cluster_nodes_offline": live, "nexo_cluster_drives_online": live,
		"nexo_cluster_drives_offline": live, "nexo_cluster_drives_healing": live, "nexo_cluster_drives_total": live,
		"nexo_cluster_capacity_raw_total_bytes": live, "nexo_cluster_capacity_raw_free_bytes": live, "nexo_cluster_capacity_raw_used_bytes": live,
		"nexo_cluster_capacity_usable_total_bytes": live, "nexo_cluster_capacity_usable_free_bytes": live,
		"nexo_cluster_sets_healthy": g, "nexo_cluster_sets_degraded": g, "nexo_cluster_sets_read_only": g, "nexo_cluster_sets_unavailable": g,
		"nexo_cluster_write_quorum": g, "nexo_cluster_read_quorum": g,
		"nexo_cluster_buckets": live, "nexo_cluster_objects": live, "nexo_cluster_usage_bytes": live,
		// Healing (node).
		"nexo_heal_drive_objects_healed": g, "nexo_heal_drive_objects_failed": g, "nexo_heal_drive_bytes_healed": g,
		"nexo_heal_drive_started_timestamp_seconds": g, "nexo_heal_drive_last_update_timestamp_seconds": g,
		"nexo_heal_jobs": g, "nexo_heal_running_objects_scanned": g, "nexo_heal_running_objects_healed": g, "nexo_heal_running_objects_failed": g,
		"nexo_mrf_queue_pending": g, "nexo_mrf_queue_leased": g, "nexo_mrf_queue_added_total": g, "nexo_mrf_queue_merged_total": g,
		"nexo_mrf_queue_dropped_total": g, "nexo_mrf_queue_healed_total": g, "nexo_mrf_queue_retried_total": g,
		// Scanner and lifecycle (node).
		"nexo_scanner_active": live, "nexo_scanner_scanning": live, "nexo_scanner_cycles_total": live,
		"nexo_scanner_last_cycle_duration_seconds": g, "nexo_scanner_last_cycle_timestamp_seconds": g, "nexo_scanner_last_cycle_buckets": g,
		"nexo_scanner_last_cycle_objects": g, "nexo_scanner_last_cycle_versions": g, "nexo_scanner_last_cycle_heal_checked": g,
		"nexo_scanner_last_cycle_heal_repaired": g, "nexo_scanner_last_cycle_heal_failed": g, "nexo_scanner_last_cycle_ilm_actions": g,
		"nexo_scanner_last_cycle_uploads_cleaned": g,
		// Lifecycle (node).
		"nexo_ilm_actions_total": g, "nexo_ilm_uploads_aborted_total": g, "nexo_ilm_errors_total": g, "nexo_ilm_versions_evaluated_total": g,
		// Notifications (node).
		"nexo_events_pending": g, "nexo_events_target_online": g, "nexo_events_sent_total": g, "nexo_events_failed_total": g,
		"nexo_events_send_duration_seconds": h, "nexo_events_published_total": g, "nexo_events_dropped_total": g,
		// Internode calls and locks (node).
		"nexo_rpc_requests_total": g, "nexo_rpc_errors_total": g, "nexo_rpc_request_duration_seconds": h, "nexo_rpc_peer_online": g,
		"nexo_lock_acquire_duration_seconds": h, "nexo_lock_acquire_timeouts_total": g, "nexo_lock_acquire_errors_total": g,
		// KMS (node).
		"nexo_kms_online": live, "nexo_kms_requests_total": live, "nexo_kms_errors_total": g, "nexo_kms_request_duration_seconds": liveH,
		// Cache and distributions (node).
		"nexo_cache_requests_total": g, "nexo_cache_hit_ratio": g, "nexo_cache_bytes_served_total": g, "nexo_cache_size_bytes": g,
		"nexo_distribution_requests_total": g, "nexo_distribution_bytes_served_total": g, "nexo_distribution_request_duration_seconds": h,
		// Process (node).
		"nexo_node_uptime_seconds": live, "nexo_node_info": live,
	}
}

// metricName matches the Nexo metric names of an expression.
var metricName = regexp.MustCompile(`\bnexo_[a-z0-9_]+`)

// referenced returns the Nexo families an expression names (the
// _bucket, _count and _sum series of a histogram name the histogram).
func referenced(t *testing.T, where, expr string) []string {
	t.Helper()
	known := families()
	var out []string
	for _, name := range metricName.FindAllString(expr, -1) {
		if _, ok := known[name]; ok {
			out = append(out, name)
			continue
		}
		found := false
		for _, suffix := range []string{"_bucket", "_count", "_sum"} {
			if base, ok := strings.CutSuffix(name, suffix); ok {
				if f, ok := known[base]; ok && f.histogram {
					out, found = append(out, base), true
				}
			}
		}
		assert.True(t, found, "%s names %s, which Nexo does not expose", where, name)
	}
	return out
}

// balanced reports an expression whose brackets match.
func balanced(expr string) bool {
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	inString := false
	for _, r := range expr {
		switch {
		case r == '"':
			inString = !inString
		case inString:
		case r == '(' || r == '[' || r == '{':
			stack = append(stack, r)
		case pairs[r] != 0:
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0 && !inString
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	require.NoError(t, dec.Decode(out), path)
}

type alertRules struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestAlertRules(t *testing.T) {
	var rules alertRules
	readYAML(t, "alerts.yml", &rules)
	require.NotEmpty(t, rules.Groups)
	names := map[string]bool{}
	used := map[string]bool{}
	for _, g := range rules.Groups {
		require.NotEmpty(t, g.Name)
		require.NotEmpty(t, g.Rules, g.Name)
		for _, r := range g.Rules {
			require.NotEmpty(t, r.Alert, "group %s: a rule without alert", g.Name)
			assert.False(t, names[r.Alert], "alert %s defined twice", r.Alert)
			names[r.Alert] = true
			assert.True(t, balanced(r.Expr), "%s: unbalanced expression", r.Alert)
			_, err := time.ParseDuration(r.For)
			assert.NoError(t, err, "%s: for %q", r.Alert, r.For)
			assert.Contains(t, []string{"critical", "warning", "info"}, r.Labels["severity"], "%s: severity", r.Alert)
			assert.NotEmpty(t, r.Annotations["summary"], "%s: summary", r.Alert)
			for _, f := range referenced(t, "alert "+r.Alert, r.Expr) {
				used[f] = true
			}
		}
	}
	// docs/05 T-8.1: drive offline, quorum at risk, healing stuck,
	// capacity above 85%, 5xx rate, event queue.
	for _, want := range []string{"NexoDriveOffline", "NexoQuorumAtRisk", "NexoHealingStalled", "NexoCapacityHigh", "NexoHigh5xxRate",
		"NexoEventQueueBacklog"} {
		assert.True(t, names[want], "missing alert %s", want)
	}
	for _, f := range []string{"nexo_drive_online", "nexo_cluster_sets_read_only", "nexo_mrf_queue_pending",
		"nexo_cluster_capacity_usable_total_bytes", "nexo_s3_requests_5xx_errors_total", "nexo_events_pending"} {
		assert.True(t, used[f], "no alert on %s", f)
	}
}

type dashboard struct {
	UID        string `json:"uid"`
	Title      string `json:"title"`
	Templating struct {
		List []struct {
			Name string `json:"name"`
		} `json:"list"`
	} `json:"templating"`
	Panels []struct {
		ID      int    `json:"id"`
		Type    string `json:"type"`
		Title   string `json:"title"`
		GridPos struct {
			X, Y, W, H int
		} `json:"gridPos"`
		Targets []struct {
			RefID string `json:"refId"`
			Expr  string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func TestGrafanaDashboard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("grafana", "dashboards", "nexo-overview.json"))
	require.NoError(t, err)
	var d dashboard
	require.NoError(t, json.Unmarshal(data, &d))
	assert.Equal(t, "nexo-overview", d.UID)
	assert.NotEmpty(t, d.Title)
	vars := map[string]bool{"__rate_interval": true, "__interval": true}
	for _, v := range d.Templating.List {
		vars[v.Name] = true
	}
	ids := map[int]bool{}
	used := map[string]bool{}
	for _, p := range d.Panels {
		assert.False(t, ids[p.ID], "panel id %d used twice", p.ID)
		ids[p.ID] = true
		assert.NotEmpty(t, p.Title, "panel %d", p.ID)
		assert.LessOrEqual(t, p.GridPos.X+p.GridPos.W, 24, "panel %q is wider than the grid", p.Title)
		if p.Type == "row" {
			continue
		}
		require.NotEmpty(t, p.Targets, "panel %q has no query", p.Title)
		refs := map[string]bool{}
		for _, tg := range p.Targets {
			assert.False(t, refs[tg.RefID], "panel %q: refId %s twice", p.Title, tg.RefID)
			refs[tg.RefID] = true
			assert.True(t, balanced(tg.Expr), "panel %q: unbalanced %s", p.Title, tg.Expr)
			for _, v := range regexp.MustCompile(`\$(\w+)`).FindAllStringSubmatch(tg.Expr, -1) {
				assert.True(t, vars[v[1]], "panel %q uses the undefined variable $%s", p.Title, v[1])
			}
			for _, f := range referenced(t, "panel "+p.Title, tg.Expr) {
				used[f] = true
			}
		}
	}
	for _, f := range []string{"nexo_s3_requests_total", "nexo_s3_request_duration_seconds", "nexo_drive_used_bytes", "nexo_cluster_nodes_online",
		"nexo_mrf_queue_pending", "nexo_events_pending", "nexo_rpc_requests_total", "nexo_lock_acquire_duration_seconds", "nexo_kms_requests_total"} {
		assert.True(t, used[f], "the dashboard does not chart %s", f)
	}
}

type promConfig struct {
	Global struct {
		ScrapeInterval     string            `yaml:"scrape_interval"`
		EvaluationInterval string            `yaml:"evaluation_interval"`
		ExternalLabels     map[string]string `yaml:"external_labels"`
	} `yaml:"global"`
	RuleFiles     []string `yaml:"rule_files"`
	ScrapeConfigs []struct {
		JobName        string `yaml:"job_name"`
		MetricsPath    string `yaml:"metrics_path"`
		ScrapeInterval string `yaml:"scrape_interval"`
		StaticConfigs  []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

func TestPrometheusConfigs(t *testing.T) {
	paths := map[string]string{"nexo-node": "/nexo/metrics/v1/node", "nexo-cluster": "/nexo/metrics/v1/cluster", "nexo-bucket": "/nexo/metrics/v1/bucket"}
	for _, file := range []string{"prometheus.single.yml", "prometheus.cluster.yml"} {
		t.Run(file, func(t *testing.T) {
			var cfg promConfig
			readYAML(t, file, &cfg)
			assert.Equal(t, []string{"/etc/prometheus/alerts.yml"}, cfg.RuleFiles)
			jobs := map[string]bool{}
			for _, sc := range cfg.ScrapeConfigs {
				jobs[sc.JobName] = true
				require.NotEmpty(t, sc.StaticConfigs, sc.JobName)
				if want, ok := paths[sc.JobName]; ok {
					assert.Equal(t, want, sc.MetricsPath, sc.JobName)
				}
			}
			for job := range paths {
				assert.True(t, jobs[job], "job %s missing (the alerts and the dashboard select it)", job)
			}
		})
	}
}

// TestFamiliesExposed scrapes a running node and checks that the live
// families of the catalog exist, so that the catalog, hence the alerts and
// the dashboard, follow the code.
func TestFamiliesExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a server")
	}
	srv := testutil.StartServer(t, testutil.WithKMS(), testutil.WithConfig(func(c *config.Static) {
		c.PrometheusAuthType = config.PrometheusAuthPublic
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := s3.NewFromConfig(aws.Config{Region: srv.Config.Region, Credentials: aws.NewCredentialsCache(staticCreds{srv.RootUser, srv.RootPassword}),
		HTTPClient: &http.Client{Timeout: 30 * time.Second}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.S3URL)
		o.UsePathStyle = true
	})
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("metrics")})
	require.NoError(t, err)
	_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("metrics"), Key: aws.String("k"), Body: strings.NewReader("hello"),
		ServerSideEncryption: types.ServerSideEncryptionAes256})
	require.NoError(t, err)
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("metrics"), Key: aws.String("k")})
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, out.Body)
	_ = out.Body.Close()

	exposed := map[string]bool{}
	typeLine := regexp.MustCompile(`(?m)^# TYPE (nexo_[a-z0-9_]+) `)
	for _, scope := range []string{"node", "cluster", "bucket"} {
		resp, err := http.Get(srv.S3URL + "/nexo/metrics/v1/" + scope)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		for _, m := range typeLine.FindAllStringSubmatch(string(body), -1) {
			exposed[m[1]] = true
		}
	}
	var missing []string
	for name, f := range families() {
		if f.live && !exposed[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	assert.Empty(t, missing, "families of the catalog a running node does not expose")
	var unknown []string
	known := families()
	for name := range exposed {
		if _, ok := known[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	t.Logf("families exposed outside the catalog (not used by the deployment files): %v", unknown)
}

type staticCreds struct{ key, secret string }

func (c staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: c.key, SecretAccessKey: c.secret, Source: "test"}, nil
}
