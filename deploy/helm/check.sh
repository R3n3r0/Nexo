#!/bin/sh
# Static checks of the Nexo chart (make helm-lint, CI job "packaging"):
# the LICENSE in the package, helm lint --strict, helm template and
# kubeconform -strict against the Kubernetes schemas (and the
# ServiceMonitor CRD) for the default values and every ci/*-values.yaml,
# on the oldest and the newest supported Kubernetes; then the endpoints
# generated for each mode and the rendering errors of invalid values.
#
#   HELM=helm KUBECONFORM=kubeconform KUBE_VERSIONS="1.27.0 1.34.0" deploy/helm/check.sh
#
# Every rendering passes --kube-version explicitly: a helm built with
# "go install" (no ldflags) assumes Kubernetes v1.20, below the kubeVersion
# of the chart. kubeconform downloads the schemas (raw.githubusercontent.com);
# without it (KUBECONFORM=) only lint, template and the assertions run.
set -eu

HELM=${HELM:-helm}
KUBECONFORM=${KUBECONFORM-kubeconform}
KUBE_VERSIONS=${KUBE_VERSIONS:-1.27.0 1.34.0}
CHART=$(cd "$(dirname "$0")/nexo" && pwd)
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
# Newest Kubernetes version: the one of the assertions below.
# shellcheck disable=SC2086 # KUBE_VERSIONS is a list
KUBE_LATEST=$(printf '%s\n' $KUBE_VERSIONS | sort -t. -k1,1n -k2,2n -k3,3n | tail -1)

out=$(mktemp)
pkg=$(mktemp -d)
trap 'rm -rf "$out" "$pkg"' EXIT

# The chart ships the license of the project (AGPL-3.0 §4, §6): its copy
# is the LICENSE of the repository, and the package carries it.
if ! cmp -s "$CHART/LICENSE" "$CHART/../../../LICENSE"; then
    echo "chart: deploy/helm/nexo/LICENSE differs from the LICENSE of the repository" >&2
    exit 1
fi
"$HELM" package "$CHART" -d "$pkg" >/dev/null
if ! tar -tzf "$pkg"/nexo-*.tgz | grep -qx 'nexo/LICENSE'; then
    echo "chart: the package has no LICENSE" >&2
    exit 1
fi

render() {
    "$HELM" template nexo "$CHART" --namespace storage --kube-version "$KUBE_LATEST" "$@"
}

# expect DESCRIPTION TEXT [helm template args...]: the rendering contains TEXT.
expect() {
    desc=$1 text=$2
    shift 2
    render "$@" >"$out"
    if ! grep -qF -- "$text" "$out"; then
        echo "chart: $desc: \"$text\" not rendered" >&2
        exit 1
    fi
}

set -- ""
for f in "$CHART"/ci/*-values.yaml; do
    set -- "$@" "$f"
done

for values in "$@"; do
    name=${values:+$(basename "$values")}
    name=${name:-defaults}
    for k in $KUBE_VERSIONS; do
        echo "== $name, Kubernetes $k"
        "$HELM" lint --strict --kube-version "$k" "$CHART" ${values:+-f "$values"}
        "$HELM" template nexo "$CHART" --namespace storage --kube-version "$k" ${values:+-f "$values"} >"$out"
        if [ -n "$KUBECONFORM" ]; then
            "$KUBECONFORM" -strict -summary -kubernetes-version "$k" \
                -schema-location default -schema-location "$CRDS" - <"$out"
        fi
    done
done

# The endpoints of "nexo server" (docs/08 §8.1) generated for every mode.
hl=nexo-hl.storage.svc.cluster.local
expect "standalone SNSD" '- "/data"'
expect "standalone SNMD" '- "/data{1...4}"' --set standalone.drives=4
expect "distributed" "- \"http://nexo-{0...3}.$hl:9000/data{1...2}\"" --set mode=distributed
expect "distributed, one drive per node" "- \"http://nexo-{0...7}.$hl:9000/data1\"" \
    --set mode=distributed,distributed.replicas=8,distributed.drivesPerNode=1
expect "distributed, two pools (pool 1)" "- \"http://nexo-{0...3}.$hl:9000/data{1...4}\"" \
    --set mode=distributed,distributed.pools=2,distributed.drivesPerNode=4
expect "distributed, two pools (pool 2)" "- \"http://nexo-{4...7}.$hl:9000/data{1...4}\"" \
    --set mode=distributed,distributed.pools=2,distributed.drivesPerNode=4
expect "distributed, TLS" "- \"https://nexo-{0...3}.$hl:9000/data{1...2}\"" \
    --set mode=distributed,tls.enabled=true,tls.secretName=nexo-tls
expect "distributed, 8 pods" "replicas: 8" --set mode=distributed,distributed.pools=2
expect "distributed, PodDisruptionBudget" "kind: PodDisruptionBudget" --set mode=distributed
expect "distributed, headless Service" "publishNotReadyAddresses: true" --set mode=distributed
expect "readiness probe" "path: /nexo/health/ready"
expect "OpenTelemetry sample ratio" 'value: "0.25"' --set config.otel.sampleRatio=0.25
expect "non-root" "runAsNonRoot: true"

# Invalid values must fail the rendering with an explicit message.
for args in "mode=cluster" "mode=distributed,distributed.replicas=1" \
    "mode=distributed,distributed.replicas=17,distributed.drivesPerNode=1" \
    "standalone.drives=17" "tls.enabled=true" "auth.rootPassword=short" \
    "kms.kes.endpoint=https://kes:7373" "config.otel.sampleRatio=2"; do
    if render --set "$args" >/dev/null 2>&1; then
        echo "expected a rendering error with --set $args" >&2
        exit 1
    fi
done
echo "chart OK"
