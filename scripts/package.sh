#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
version=$(cat VERSION)
mkdir -p dist
rm -f dist/hopport-v"$version"-* dist/SHA256SUMS

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}
  arch=${target#*/}
  name="hopport-v${version}-${os}-${arch}"
  stage="dist/.${name}"
  rm -rf "$stage"
  mkdir -p "$stage/assets"
  cp README.md LICENSE "${stage}/"
  cp assets/hopport-logo.png "${stage}/assets/"

  if [[ $os == windows ]]; then
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=${version}" -o "${stage}/hopport.exe" .
    cp run.cmd "$stage/"
    (cd "$stage" && zip -q -r "../${name}.zip" .)
  else
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=${version}" -o "${stage}/hopport" .
    cp run.sh "$stage/"
    chmod +x "${stage}/hopport" "${stage}/run.sh"
    tar -C "$stage" -czf "dist/${name}.tar.gz" .
  fi
  rm -rf "$stage"
done

(cd dist && sha256sum hopport-v"$version"-* > SHA256SUMS)
