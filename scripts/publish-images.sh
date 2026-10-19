#!/usr/bin/env bash
# Builds the multi-arch nexo, nexoctl and nexo-media-worker images on this machine and pushes them
# to Docker Hub (the only registry of the Nexo images), using credentials from
# the environment. Use it when you want to publish without a tag-driven
# release.
#
#   DOCKERHUB_USERNAME=paolor DOCKERHUB_TOKEN=dckr_pat_... VERSION=v1.0.0 scripts/publish-images.sh
#
# Docker Hub:
#   DOCKERHUB_USERNAME, DOCKERHUB_TOKEN  required unless DRY_RUN=true (access
#                                        token with Read & Write scope)
#   DOCKERHUB_NAMESPACE                  default: paolor (lowercased); images
#                                        docker.io/<namespace>/nexo, nexoctl and
#                                        nexo-media-worker
#
# Options:
#   VERSION     image tag; default: git describe (v1.2.3, v1.2.3-4-gabcdef)
#   PLATFORMS   default: linux/amd64,linux/arm64
#   IMAGES      default: "nexo nexoctl nexo-media-worker"
#   LATEST      "true" also tags :latest and :vX.Y (default: true for a plain
#               vX.Y.Z version, false otherwise)
#   SIGN        "true" signs the pushed digests with cosign (keyless or
#               COSIGN_KEY); default: false
#   DRY_RUN     "true" builds for the local platform without logging in or
#               pushing
#
# Requirements: docker with the buildx plugin (and QEMU binfmt for foreign
# platforms: docker run --privileged --rm tonistiigi/binfmt --install all).
set -euo pipefail

cd "$(dirname "$0")/.."

die() { echo "publish-images: $*" >&2; exit 1; }
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

command -v docker >/dev/null || die "docker is not installed"
docker buildx version >/dev/null 2>&1 || die "the docker buildx plugin is required"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
[[ $VERSION == *-dirty ]] && [[ ${DRY_RUN:-false} != true ]] &&
	die "the working tree has uncommitted changes ($VERSION); commit them or set VERSION"
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
PLATFORMS=${PLATFORMS:-linux/amd64,linux/arm64}
IMAGES=${IMAGES:-nexo nexoctl nexo-media-worker}
DRY_RUN=${DRY_RUN:-false}
SIGN=${SIGN:-false}

if [[ -z ${LATEST:-} ]]; then
	if [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then LATEST=true; else LATEST=false; fi
fi

# Docker Hub repository prefix of the images.
NAMESPACE=$(lower "${DOCKERHUB_NAMESPACE:-paolor}")
[[ -n $NAMESPACE ]] || NAMESPACE=paolor
repo="docker.io/$NAMESPACE"
if [[ $DRY_RUN != true ]]; then
	[[ -n ${DOCKERHUB_USERNAME:-} && -n ${DOCKERHUB_TOKEN:-} ]] ||
		die "DOCKERHUB_USERNAME and DOCKERHUB_TOKEN are required to push to Docker Hub (or set DRY_RUN=true)"
	printf '%s' "$DOCKERHUB_TOKEN" | docker login docker.io -u "$DOCKERHUB_USERNAME" --password-stdin
fi

tags_for() { # image -> one --tag per tag
	local image=$1
	echo "--tag=$repo/$image:$VERSION"
	if [[ $LATEST == true ]]; then
		echo "--tag=$repo/$image:latest"
		echo "--tag=$repo/$image:${VERSION%.*}"
	fi
}

mkdir -p bin
builder=nexo-publish
docker buildx inspect "$builder" >/dev/null 2>&1 ||
	docker buildx create --name "$builder" --driver docker-container >/dev/null

echo "publish-images: version $VERSION ($COMMIT), platforms $PLATFORMS"
echo "publish-images: repository $repo (Docker Hub)"

for image in $IMAGES; do
	case $image in
	nexo) dockerfile=Dockerfile ;;
	nexoctl) dockerfile=Dockerfile.nexoctl ;;
	nexo-media-worker) dockerfile=Dockerfile.media-worker ;;
	*) die "unknown image $image" ;;
	esac

	mapfile -t tags < <(tags_for "$image")
	if [[ $DRY_RUN == true ]]; then
		output=(--load)
		platforms=$(docker version -f '{{.Server.Os}}/{{.Server.Arch}}')
	else
		output=(--push --sbom=true --provenance=mode=max)
		platforms=$PLATFORMS
	fi

	echo "publish-images: building $image"
	docker buildx build --builder "$builder" \
		--file "$dockerfile" --platform "$platforms" \
		--build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --build-arg "DATE=$DATE" \
		--metadata-file "bin/publish-$image.json" \
		"${tags[@]}" "${output[@]}" .

	if [[ $DRY_RUN != true && $SIGN == true ]]; then
		command -v cosign >/dev/null || die "SIGN=true needs cosign"
		digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "bin/publish-$image.json")
		cosign sign --yes "$repo/$image@$digest"
	fi
done

if [[ $DRY_RUN == true ]]; then
	echo "publish-images: done (dry run, loaded locally, nothing pushed)"
	exit 0
fi
echo "publish-images: done"
for image in $IMAGES; do echo "  docker pull $repo/$image:$VERSION"; done
