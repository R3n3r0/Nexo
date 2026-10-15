package event

import "net/http"

// The constructors below share the transports of the notification targets
// with the other services that deliver to the same kinds of destinations
// with the same settings (the audit log targets audit_webhook and
// audit_kafka): one TLS, redirect and SASL policy for every webhook and
// Kafka producer of the server.

// NewWebhookClient returns the HTTP client of a webhook target configured
// by cfg: its TLS settings (client certificate, tls_skip_verify) and no
// redirect ever followed (a 3xx answer is a failed delivery).
func NewWebhookClient(cfg WebhookConfig) (*http.Client, error) { return webhookClient(cfg) }

// NewKafkaDialer returns the dialer of the Kafka targets (kafka-go
// writers with the TLS and SASL settings of KafkaConfig).
func NewKafkaDialer() KafkaDialer { return kafkaDialer{} }
