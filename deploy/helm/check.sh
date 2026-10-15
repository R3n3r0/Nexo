#!/bin/sh
# Static checks of the Nexo chart (make helm-lint, CI job "packaging"):
# helm lint --strict, helm template and kubeconform -strict against the
# Kubernetes schemas (and the ServiceMonitor CRD) for the default values and
# every ci/*-values.yaml, on the oldest and the newest supported Kubernetes.
#
#   HELM=helm KUBECONFORM=kubeconform KUBE_VERSIONS="1.27.0 1.34.0" deploy/helm/check.sh
#
# kubeconform downloads the schemas (raw.githubusercontent.com); without it
# (KUBECONFORM=) only lint and template run.
set -eu

HELM=${HELM:-helm}
KUBECONFORM=${KUBECONFORM-kubeconform}
KUBE_VERSIONS=${KUBE_VERSIONS:-1.27.0 1.34.0}
CHART=$(cd "$(dirname "$0")/nexo" && pwd)
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

out=$(mktemp)
trap 'rm -f "$out"' EXIT

set -- ""
for f in "$CHART"/ci/*-values.yaml; do
    set -- "$@" "$f"
done

for values in "$@"; do
    name=${values:+$(basename "$values")}
    name=${name:-defaults}
    echo "== $name"
    "$HELM" lint --strict "$CHART" ${values:+-f "$values"}
    "$HELM" template nexo "$CHART" --namespace storage ${values:+-f "$values"} >"$out"
    if [ -n "$KUBECONFORM" ]; then
        for k in $KUBE_VERSIONS; do
            "$KUBECONFORM" -strict -summary -kubernetes-version "$k" \
                -schema-location default -schema-location "$CRDS" - <"$out"
        done
    fi
done

# Invalid values must fail the rendering with an explicit message.
for args in "mode=cluster" "mode=distributed,distributed.replicas=1" \
    "mode=distributed,distributed.replicas=17,distributed.drivesPerNode=1" \
    "standalone.drives=17" "tls.enabled=true" "auth.rootPassword=short"; do
    if "$HELM" template nexo "$CHART" --set "$args" >/dev/null 2>&1; then
        echo "expected a rendering error with --set $args" >&2
        exit 1
    fi
done
echo "chart OK"
