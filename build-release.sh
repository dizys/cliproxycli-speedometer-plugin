#!/usr/bin/env bash
# Build the release artifacts the CLIProxyAPI plugin store expects.
#
# Each platform ships one zip holding the shared library at the zip root, plus a
# single sha256sum-format checksums.txt covering every zip. The plugin store
# picks the archive named <id>_<version>_<goos>_<goarch>.zip from the release
# whose tag is v<version>.
#
# Cross-compiling a c-shared library needs a C toolchain for the target. The
# script builds linux/amd64, linux/arm64 and darwin/arm64 and skips any platform
# whose toolchain is missing, so a bare checkout still produces a release.
set -euo pipefail

cd "$(dirname "$0")"

PLUGIN_ID="cliproxy-meter"
VERSION="${VERSION:-0.1.0}"
# zig cc is the portable way to cross-compile cgo; a native cross gcc also works.
CC_AMD64="${CC_AMD64:-}"
CC_ARM64="${CC_ARM64:-aarch64-linux-gnu-gcc}"
CC_DARWIN_ARM64="${CC_DARWIN_ARM64:-aarch64-apple-darwin-clang}"

OUT_DIR="${OUT_DIR:-dist}"
rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}/tmp"

have_cc() {
    command -v "$1" >/dev/null 2>&1
}

build_platform() {
    local goos="$1" goarch="$2" cc="$3"
    local lib_ext=".so"
    [ "${goos}" = "darwin" ] && lib_ext=".dylib"

    if [ -n "${cc}" ] && ! have_cc "${cc}"; then
        echo "skip ${goos}/${goarch}: compiler ${cc} not found" >&2
        return 0
    fi

    local stage="${OUT_DIR}/tmp/${goos}_${goarch}"
    mkdir -p "${stage}"
    # The zip must contain the versioned library name at its root:
    # <id>-v<version>.<ext> for version-matched installs.
    local lib="${PLUGIN_ID}-v${VERSION}${lib_ext}"

    echo "building ${goos}/${goarch} with ${cc:-default cc}"
    CC="${cc}" CGO_ENABLED=1 GOOS="${goos}" GOARCH="${goarch}" \
        go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o "${stage}/${lib}" .

    local zip_name="${PLUGIN_ID}_${VERSION}_${goos}_${goarch}.zip"
    (cd "${stage}" && zip -q -X "../${zip_name}" "${lib}")
    mv "${OUT_DIR}/tmp/${zip_name}" "${OUT_DIR}/${zip_name}"
}

build_platform linux amd64 "${CC_AMD64}"
build_platform linux arm64 "${CC_ARM64}"
build_platform darwin arm64 "${CC_DARWIN_ARM64}"

rm -rf "${OUT_DIR}/tmp"

if ! ls "${OUT_DIR}"/*.zip >/dev/null 2>&1; then
    echo "no archives built; install a cross compiler (aarch64-linux-gnu-gcc) or build on the target" >&2
    exit 1
fi

# The host looks checksums up by exact asset name, so no "./" prefix.
(cd "${OUT_DIR}" && sha256sum ./*.zip | sed 's|  \./|  |' > checksums.txt)
echo "---"
(cd "${OUT_DIR}" && ls -l ./*.zip checksums.txt)
