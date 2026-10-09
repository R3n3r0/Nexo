// Package cluster holds the multi-process cluster tests of Nexo (docs/06
// §6.3, TC-*): every test builds a deployment of several bin/nexo
// processes on 127.0.0.1, each on its own S3 and console ports with its
// drives in a temporary directory, and drives it through real clients
// (aws-sdk-go-v2 and the Admin API client) while it stops, pauses, kills
// and restarts the processes. A deterministic workload generator (seeded)
// and a checker compare the final state of every key (key → sha256) with
// the ledger of the operations the cluster confirmed.
//
// The tests are behind the "cluster" build tag: they are not part of the
// standard go test ./... run and need a Unix system (signals, Linux for
// the multi-pool layouts). Run them with
//
//	make test-cluster
//	go test -tags cluster -count=1 ./test/cluster/...
//
// The server binary is built with the test hooks (build tag nexotest,
// cmd/nexo/testhooks_nexotest.go): NEXO_TEST_CLOCK_OFFSET shifts the
// clock of a node (TC-13) and NEXO_TEST_DIAL_MAP routes its internode
// connections through the TCP proxies of the harness (netem_test.go),
// which cut the links of a real partition with both sides running
// (TC-07) or add latency and lost segments (TC-08), without Docker nor
// toxiproxy. TC-12 builds two versions of the binary and upgrades the
// nodes one at a time under load; TC-15 runs the workload under random
// chaos (kills, pauses, partitions) for NEXO_CLUSTER_SOAK (20s by
// default, 10m in the nightly CI job).
//
// NEXO_CLUSTER_BIN reuses a prebuilt server binary (built with -tags
// nexotest for the tests that use the hooks) instead of building one,
// NEXO_CLUSTER_SEED replays a workload.
package cluster
