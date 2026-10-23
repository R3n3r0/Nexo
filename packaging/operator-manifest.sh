#!/bin/sh
# Renders the installation manifest of the Kubernetes operator (the
# resources of deploy/operator/kustomization.yaml, in order) with the image
# of a release, for kubectl apply -f. GoReleaser attaches it to the release
# as nexo-operator.yaml (.goreleaser.yaml); make operator-check validates it.
#
#   packaging/operator-manifest.sh v1.1.0 [docker.io/<namespace>] > nexo-operator.yaml
set -eu

tag=${1:?usage: operator-manifest.sh TAG [REGISTRY]}
registry=${2:-docker.io/paolor}
dir=$(cd "$(dirname "$0")/../deploy/operator" && pwd)

# The resources listed by the kustomization, in its order.
resources=$(sed -n '/^resources:/,/^[^ ]/s/^  - //p' "$dir/kustomization.yaml")
[ -n "$resources" ] || { echo "operator-manifest: no resources in $dir/kustomization.yaml" >&2; exit 1; }

echo "# Nexo operator $tag: kubectl apply --server-side -f nexo-operator.yaml"
for r in $resources; do
    echo "---"
    # Without the document markers and comments of the sources; the image
    # of the operator pinned to the release.
    sed -e '/^---$/d' -e '/^#/d' \
        -e "s#image: docker\.io/paolor/nexo-operator:latest#image: ${registry}/nexo-operator:${tag}#" "$dir/$r"
done
