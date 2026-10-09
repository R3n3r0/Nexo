// Package proxy holds the tests of Nexo behind several reverse proxies
// (docs/10 §10.2 "Dietro uno o più proxy", §10.6 TCD-01..03, docs/11
// §11.12 TCC-C-05, docs/09 TPS-19 on the custom domains of the
// distributions).
//
// Every test starts a deployment in the test process (a cluster of 4 nodes
// x 2 drives, or a single node with one or four drives) and two edge
// proxies in front of it, the in-process stand-ins of the nginx and
// HAProxy services of deploy/docker/compose.multi-proxy.yml: Go reverse
// proxies (httputil.ReverseProxy) terminating TLS for their own domains,
// each with its own origin secret and its own way of telling Nexo who the
// client is:
//
//   - edge A serves *.alpha.test (S3 API, console, distributions) and
//     forwards X-Forwarded-For/Proto/Host, as nginx, Caddy and Traefik do;
//   - edge B serves *.beta.test (distributions only) and opens every
//     upstream connection with a PROXY protocol v2 header carrying the
//     client address and the SSL information of its TLS connection, as
//     HAProxy does with send-proxy-v2-ssl.
//
// Both balance over the nodes, retry an idempotent request on another
// node when one refuses the connection or answers 502/503/504 (nginx
// proxy_next_upstream) and probe /nexo/health/ready. The domains are not
// resolved: the clients dial the edge whatever the host of the URL, which
// is what a DNS record pointing the domains to the proxy does.
//
// The tests are behind the "proxy" build tag:
//
//	make test-proxy
//	go test -tags proxy -race -count=1 ./test/proxy/...
//
// They bind the loopback addresses 127.0.0.11 and 127.0.0.12 (the edges,
// the trusted proxies of the deployment) and 127.0.0.6x/7x (the viewers):
// Linux routes the whole 127.0.0.0/8 to the loopback interface; elsewhere
// the tests are skipped.
package proxy
