#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  echo "usage: $0 <xgo-image> <module> <plugin-id> <version>" >&2
  exit 2
fi

image="$1"
module="$2"
plugin_id="$3"
version="$4"
dist_dir="${DIST_DIR:-dist}"

if [[ ! "$plugin_id" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
  echo "invalid plugin id: $plugin_id" >&2
  exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid version: $version" >&2
  exit 2
fi

container_id=""
cleanup() {
  status=$?
  if [[ -n "$container_id" ]] && ! docker rm -f "$container_id" >/dev/null; then
    echo "failed to remove xgo container $container_id" >&2
    if [[ "$status" -eq 0 ]]; then
      status=1
    fi
  fi
  trap - EXIT
  exit "$status"
}
trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT

mkdir -p "$dist_dir/raw"
if [[ -n "$(find "$dist_dir/raw" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "$dist_dir/raw must be empty" >&2
  exit 1
fi

container_id="$(docker create \
  --entrypoint /bin/sh \
  -e GO111MODULE=on \
  -e REPO_REMOTE= \
  -e REPO_BRANCH= \
  -e PACK= \
  -e DEPS= \
  -e ARGS= \
  -e "OUT=$plugin_id" \
  -e FLAG_V=false \
  -e FLAG_X=false \
  -e FLAG_RACE=false \
  -e FLAG_TAGS= \
  -e "FLAG_LDFLAGS=-s -w -X main.pluginVersion=$version" \
  -e FLAG_GCFLAGS= \
  -e FLAG_BUILDMODE=c-shared \
  -e FLAG_TRIMPATH=true \
  -e FLAG_BUILDVCS=false \
  -e FLAG_MOD= \
  -e FLAG_OBFUSCATE=false \
  -e GARBLE_FLAGS= \
  -e 'TARGETS=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64' \
  -e "GOPROXY=$(go env GOPROXY)" \
  -e "GOPRIVATE=$(go env GOPRIVATE)" \
  -e "GOEXPERIMENT=$(go env GOEXPERIMENT)" \
  "$image" -c 'while :; do sleep 3600; done')"

docker start "$container_id" >/dev/null
docker exec "$container_id" mkdir -p /source /build /gocache /deps-cache /hooksdir /release
git archive --format=tar HEAD | docker cp - "${container_id}:/source"
docker exec "$container_id" cp /source/.github/xgo/setup.sh /hooksdir/setup.sh
docker exec "$container_id" /build.sh "$module"
docker cp "${container_id}:/build/." "$dist_dir/raw"

targets=(
  "linux|amd64|$dist_dir/raw/$plugin_id-linux-amd64.so|so"
  "linux|arm64|$dist_dir/raw/$plugin_id-linux-arm64.so|so"
  "darwin|amd64|$dist_dir/raw/$plugin_id-darwin-*-amd64.dylib|dylib"
  "darwin|arm64|$dist_dir/raw/$plugin_id-darwin-*-arm64.dylib|dylib"
  "windows|amd64|$dist_dir/raw/$plugin_id-windows-*-amd64.dll|dll"
)

for target in "${targets[@]}"; do
  IFS='|' read -r goos goarch pattern extension <<< "$target"
  mapfile -t matches < <(compgen -G "$pattern" || true)
  if [[ "${#matches[@]}" -ne 1 ]]; then
    echo "expected one cross-build output for $goos/$goarch, found ${#matches[@]}" >&2
    exit 1
  fi

  source_library="${matches[0]}"
  build_metadata="$(go version -m "$source_library")"
  grep -Fq $'\tbuild\tGOOS='"$goos" <<< "$build_metadata"
  grep -Fq $'\tbuild\tGOARCH='"$goarch" <<< "$build_metadata"

  package_dir="/release/package-${goos}-${goarch}"
  docker exec "$container_id" mkdir -p "$package_dir"
  docker cp "$source_library" "${container_id}:${package_dir}/${plugin_id}.${extension}"
done

docker exec \
  --env "VERSION=$version" \
  --env "PLUGIN_ID=$plugin_id" \
  "$container_id" \
  bash -c '
    set -euo pipefail
    for package_dir in /release/package-*; do
      target="${package_dir##*/package-}"
      goos="${target%-*}"
      goarch="${target##*-}"
      case "$goos" in
        windows) extension=dll ;;
        darwin) extension=dylib ;;
        *) extension=so ;;
      esac
      library="$PLUGIN_ID.$extension"
      archive="/release/${PLUGIN_ID}_${VERSION}_${goos}_${goarch}.zip"
      (cd "$package_dir" && zip -q "$archive" "$library")
      [[ "$(unzip -Z1 "$archive")" == "$library" ]]
    done
    cd /release
    sha256sum "${PLUGIN_ID}"_*.zip > checksums.txt
  '

expected=(
  "${plugin_id}_${version}_linux_amd64.zip"
  "${plugin_id}_${version}_linux_arm64.zip"
  "${plugin_id}_${version}_darwin_amd64.zip"
  "${plugin_id}_${version}_darwin_arm64.zip"
  "${plugin_id}_${version}_windows_amd64.zip"
)
for archive in "${expected[@]}"; do
  docker cp "${container_id}:/release/${archive}" "$dist_dir/$archive"
done
docker cp "${container_id}:/release/checksums.txt" "$dist_dir/checksums.txt"

mapfile -t archives < <(find "$dist_dir" -maxdepth 1 -type f -name "${plugin_id}_*.zip" -printf '%f\n' | sort)
if [[ "${#archives[@]}" -ne 5 ]]; then
  echo "expected five release archives, found ${#archives[@]}" >&2
  exit 1
fi
(cd "$dist_dir" && sha256sum -c checksums.txt)
