// Package compose holds the tests of the Docker Compose deployments of
// deploy/docker (docs/08 §8.6): TM-06 (a container of the cluster stopped
// under load: no client error) and TM-09 (presigned and share links
// generated behind the nginx load balancer, downloaded from outside).
//
// They are behind the "compose" build tag and drive a running
// deployment: the nightly CI job starts compose.cluster.yml with the image
// under test, then runs
//
//	NEXO_COMPOSE_FILE=deploy/docker/compose.cluster.yml \
//	  go test -tags compose -count=1 -v ./test/compose/...
//
// NEXO_COMPOSE_S3 and NEXO_COMPOSE_CONSOLE are the URLs of the load
// balancer (default http://localhost:9000 and http://localhost:9001),
// NEXO_COMPOSE_USER and NEXO_COMPOSE_PASSWORD the root credentials of
// nexo.env (default user admin; the password is required, the example
// leaves it empty), NEXO_COMPOSE_STOP the
// service stopped by TM-06 (default nexo3).
package compose
