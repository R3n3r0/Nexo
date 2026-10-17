#!/usr/bin/env bash
# Builds the multi-arch nexo and nexoctl images on this machine and pushes them
# to GitHub Container Registry and/or Docker Hub, using credentials from the
# environment. Use it when you want to publish without a tag-driven release.
#
#   VERSION=v1.0.0 scripts/publish-images.sh
#
# Registries (each one is used only when its credentials are set):
#   GHCR_USERNAME, GHCR_TOKEN            ghcr.io (token with write:packages)
#   GHCR_NAMESPACE                       default: GHCR_USERNAME, lowercased
#   DOCKERHUB_USERNAME, DOCKERHUB_TOKEN  docker.io (access token, Read & Write)
#   DOCKERHUB_NAMESPACE                  default: DOCKERHUB_USERNAME, lowercased
#
# Options:
#   VERSION     image tag; default: git describe (v1.2.3, v1.2.3-4-gabcdef)
#   PLATFORMS   default: linux/amd64,linux/arm64
#   IMAGES      default: "nexo nexoctl"
#   LATEST      "true" also tags :latest and :vX.Y (default: true for a plain
#               vX.Y.Z version, false otherwise)
#   SIGN        "true" signs the pushed digests with cosign (keyless or
#               COSIGN_KEY); default: false
#   DRY_RUN     "true" builds for the local platform without pushing
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
IMAGES=${IMAGES:-nexo nexoctl}
DRY_RUN=${DRY_RUN:-false}
SIGN=${SIGN:-false}

if [[ -z ${LATEST:-} ]]; then
	if [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then LATEST=true; else LATEST=false; fi
fi

# Repositories to push to, one per configured registry.
repos=()
if [[ -n ${GHCR_USERNAME:-} && -n ${GHCR_TOKEN:-} ]]; then
	repos+=("ghcr.io/$(lower "${GHCR_NAMESPACE:-$GHCR_USERNAME}")")
	[[ $DRY_RUN == true ]] || printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USERNAME" --password-stdin
fi
if [[ -n ${DOCKERHUB_USERNAME:-} && -n ${DOCKERHUB_TOKEN:-} ]]; then
	repos+=("docker.io/$(lower "${DOCKERHUB_NAMESPACE:-$DOCKERHUB_USERNAME}")")
	[[ $DRY_RUN == true ]] || printf '%s' "$DOCKERHUB_TOKEN" | docker login docker.io -u "$DOCKERHUB_USERNAME" --password-stdin
fi
if [[ ${#repos[@]} -eq 0 && $DRY_RUN != true ]]; then
	die "no registry configured: set GHCR_USERNAME/GHCR_TOKEN and/or DOCKERHUB_USERNAME/DOCKERHUB_TOKEN"
fi
[[ ${#repos[@]} -gt 0 ]] || repos=("local")

tags_for() { # image -> one --tag per repository and tag
	local image=$1 repo
	for repo in "${repos[@]}"; do
		echo "--tag=$repo/$image:$VERSION"
		if [[ $LATEST == true ]]; then
			echo "--tag=$repo/$image:latest"
			echo "--tag=$repo/$image:${VERSION%.*}"
		fi
	done
}

mkdir -p bin
builder=nexo-publish
docker buildx inspect "$builder" >/dev/null 2>&1 ||
	docker buildx create --name "$builder" --driver docker-container >/dev/null

echo "publish-images: version $VERSION ($COMMIT), platforms $PLATFORMS"
echo "publish-images: repositories ${repos[*]}"

for image in $IMAGES; do
	case $image in
	nexo) dockerfile=Dockerfile ;;
	nexoctl) dockerfile=Dockerfile.nexoctl ;;
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
		for repo in "${repos[@]}"; do
			cosign sign --yes "$repo/$image@$digest"
		done
	fi
done

echo "publish-images: done"
for repo in "${repos[@]}"; do
	[[ $repo == local ]] && continue
	for image in $IMAGES; do echo "  docker pull $repo/$image:$VERSION"; done
done
