#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

if [[ ! -f web/dist/index.html ]]; then
  echo 'Build the web assets first: cd web && pnpm install --frozen-lockfile && pnpm build' >&2
  exit 1
fi

commit=$(git rev-parse HEAD)
version="main-${commit:0:12}"
# Use the source timestamp so rebuilding the same commit is reproducible.
build_time=$(git show -s --format=%cI HEAD)
output_dir="$repo_root/dist/linux"
mkdir -p "$output_dir"
binaries=()

for arch in amd64 arm64; do
  for variant in web no-web; do
    build_tags=goolm
    suffix=
    if [[ "$variant" == no-web ]]; then
      build_tags=goolm,no_web
      suffix=-no-web
    fi
    binary="$output_dir/cc-connect-linux-$arch$suffix"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
      -mod=readonly -trimpath -tags "$build_tags" \
      -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.buildTime=$build_time" \
      -o "$binary" ./cmd/cc-connect

    case "$arch" in
      amd64) expected_machine=x86-64 ;;
      arm64) expected_machine='ARM aarch64' ;;
    esac
    binary_info=$(file --brief "$binary")
    case "$binary_info" in
      *"$expected_machine"*'statically linked'*) printf '%s: %s\n' "$binary" "$binary_info" ;;
      *) printf 'Unexpected binary format: %s\n' "$binary_info" >&2; exit 1 ;;
    esac
    binaries+=("${binary##*/}")

    # Exercise both variants when their architecture matches the build machine.
    case "$(uname -s)/$(uname -m)/$arch" in
      Linux/x86_64/amd64|Linux/aarch64/arm64) "$binary" --version ;;
    esac
  done
done

printf 'version=%s\ncommit=%s\nbuild_time=%s\nvariants=web,no-web\n' "$version" "$commit" "$build_time" > "$output_dir/build-info.txt"
cd "$output_dir"
sha256sum "${binaries[@]}" build-info.txt > SHA256SUMS
sha256sum -c SHA256SUMS
