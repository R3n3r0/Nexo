// Package proxy holds the tests of Nexo behind several reverse proxies
// (docs/10 §10.2 "Dietro uno o più proxy", §10.6 TCD-01..03, docs/11
// §11.12 TCC-C-05, docs/09 TPS-19 on the custom domains of the
// distributions).
//
// Every test starts a deployment in the test process (a single node with
// one or four drives, or a cluster of 4 nodes x 2 drives) and two edge
// proxies in front of it, the in-process stand-ins of the nginx and
// HAProxy services of deploy/docker/compose.multi-proxy.yml: Go reverse
// proxies (httputil.ReverseProxy) terminating TLS for their own domains,
// each with its own origin secret and its own way of telling Nexo who the
// client is:
//
//   - edge A serves *.alpha.test (the S3 API on s3.alpha.test, the
//     console on console.alpha.test, the distributions) and forwards
//     X-Forwarded-For/Proto/Host, as nginx, Caddy and Traefik do;
//   - edge B serves *.beta.test (distributions only) and opens every
//     upstream connection with a PROXY protocol v2 header carrying the
//     client address and the SSL information of its TLS connection, as
//     HAProxy does with send-proxy-v2-ssl.
//
// The nodes trust both edges (NEXO_TRUSTED_PROXIES), accept the PROXY
// protocol on the S3 listener (NEXO_PROXY_PROTOCOL=s3) and know both
// origin secrets (NEXO_SECRET_PROXY_A, NEXO_SECRET_PROXY_B), which the
// distributions reference as ${secret:proxy-a} and ${secret:proxy-b}.
// Both edges balance over the nodes (round robin), retry on another node
// a request refused at the connection, and an idempotent one that failed
// or got 502/503/504 (nginx proxy_next_upstream), and probe
// /nexo/health/ready. The domains are not resolved: the clients dial the
// edge whatever the host of the URL, which is what a DNS record pointing
// the domains to the proxy does.
//
// The tests are behind the "proxy" build tag:
//
//	make test-proxy
//	go test -tags proxy -race -count=1 ./test/proxy/...
//
// NEXO_PROXY_MODES restricts the deployment modes (snsd, snmd, cluster,
// comma separated; default all). The tests bind the loopback addresses
// 127.0.0.11 and 127.0.0.12 (the edges) and 127.0.0.61, .62 and .66 (the
// viewers): Linux routes the whole 127.0.0.0/8 to the loopback interface;
// where those addresses cannot be bound the tests are skipped.
package proxy
