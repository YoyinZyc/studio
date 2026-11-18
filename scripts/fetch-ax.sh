#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Downloads the `ax` CLI that ships inside the app into
# vendor/ax/<os>-<arch>/ax, where electron-builder.yml picks it up for the
# platform being packaged. Run it before `npm run dist`.
#
# The release and the SHA-256 of each archive are pinned here, so a build
# never ships an `ax` other than the one this commit names. To move to another
# release, change AX_VERSION and paste that release's checksums.txt below.

set -euo pipefail

# TODO: Point at google/ax once it publishes releases; this is a test fork.
AX_REPO="YoyinZyc/ax"
AX_VERSION="v0.0.5-test"
CHECKSUMS="
3aecf1a080fe6636f3c8a4fe97648838744202e23c4ea8ea009192342916120a  ax_darwin_amd64.tar.gz
0662c5793d966b0055b809e71d31035357061c4ec813ebcb48997c49ff2c73ff  ax_darwin_arm64.tar.gz
083b9d32e403f41c7f163437d1ef45daa6230fd6551350f8650c74a03f569bee  ax_linux_amd64.tar.gz
dfd42fd7731e3cd1927d39b6a10f534bbd1fc7c65b058c7a82ab1fb843c0f522  ax_linux_arm64.tar.gz
"

root="$(cd "$(dirname "$0")/.." && pwd)"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# fetch <ax os> <ax arch> <electron-builder os> <electron-builder arch>
fetch() {
  local asset="ax_$1_$2.tar.gz"
  local dest="${root}/vendor/ax/$3-$4"
  local expected actual tmp

  expected="$(awk -v asset="${asset}" '$2 == asset { print $1 }' <<<"${CHECKSUMS}")"
  if [[ -z "${expected}" ]]; then
    echo "Error: no pinned SHA-256 for ${asset}." >&2
    return 1
  fi

  tmp="$(mktemp -d)"
  echo "==> Downloading ${asset} from ${AX_REPO} ${AX_VERSION}..."
  curl -fsSL --retry 3 -o "${tmp}/${asset}" \
    "https://github.com/${AX_REPO}/releases/download/${AX_VERSION}/${asset}"

  actual="$(sha256_file "${tmp}/${asset}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "Error: SHA-256 mismatch for ${asset} (expected ${expected}, got ${actual})." >&2
    rm -rf "${tmp}"
    return 1
  fi

  tar -xzf "${tmp}/${asset}" -C "${tmp}"
  if [[ ! -f "${tmp}/ax" ]]; then
    echo "Error: ${asset} did not contain an 'ax' binary." >&2
    rm -rf "${tmp}"
    return 1
  fi

  rm -rf "${dest}"
  mkdir -p "${dest}"
  mv "${tmp}/ax" "${dest}/ax"
  chmod 0755 "${dest}/ax"
  rm -rf "${tmp}"
  echo "==> Wrote ${dest}/ax"
}

case "$(uname -s)" in
  Darwin)
    fetch darwin arm64 mac arm64
    fetch darwin amd64 mac x64
    ;;
  Linux)
    fetch linux amd64 linux x64
    fetch linux arm64 linux arm64
    ;;
  *)
    # AX has no Windows build. Leave an empty directory so packaging has
    # something to copy; the app then looks for `ax` on PATH.
    echo "==> No ax build for $(uname -s); the app will not include one."
    mkdir -p "${root}/vendor/ax/win-x64"
    ;;
esac
