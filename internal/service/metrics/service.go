// Package metrics wraps the Prometheus registry so that services register
// collectors through an interface and transports expose them over HTTP.
//
//go:generate mockery --name Registry --output ./mock --outpkg mock --with-expecter
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Collector is a Prometheus collector.
type Collector = prometheus.Collector

// Registry registers collectors and gathers their metrics. It is satisfied
// by *prometheus.Registry.
type Registry interface {
	prometheus.Registerer
	prometheus.Gatherer
}

// Namespace is the prefix of every Nexo metric.
const Namespace = "nexo"
