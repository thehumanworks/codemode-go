#!/usr/bin/env bash
# Rebuilds wasm/qjs.wasm: QuickJS-ng without quickjs-libc, with the
# fastschema/qjs glue from wasm/glue. See wasm/README.md.
#
# Needs: git, cmake, ninja, wasm-opt (binaryen) and a wasi-sdk
# (WASI_SDK=/path/to/wasi-sdk-NN.N-...). `mise install` in the repository
# provides all of them and sets WASI_SDK; `mise run wasm` runs this script.
# Run from anywhere. The output is reproducible: same inputs, same bytes.
set -euo pipefail

QUICKJS_NG_COMMIT=d01ca4491fb24ccfeccb4c7394e28a3b21fd5986 # fastschema/qjs v0.0.6's submodule pin
: "${WASI_SDK:?set WASI_SDK to the wasi-sdk directory (its share/cmake holds wasi-sdk-p1.cmake), or run 'mise install' and 'mise run wasm'}"
for tool in git cmake ninja wasm-opt; do
  command -v "$tool" >/dev/null || { echo "$tool not found on PATH ('mise install' provides it)" >&2; exit 1; }
done

here=$(cd "$(dirname "$0")" && pwd)
build="$here/build"
rm -rf "$build"
mkdir -p "$build"
cp "$here"/glue/*.c "$here"/glue/*.h "$here"/glue/qjswasm.cmake "$build/"

git clone -q https://github.com/quickjs-ng/quickjs "$build/quickjs"
git -C "$build/quickjs" checkout -q "$QUICKJS_NG_COMMIT"

toolchain="$WASI_SDK/share/cmake/wasi-sdk-p1.cmake"
[ -f "$toolchain" ] || toolchain="$WASI_SDK/share/cmake/wasi-sdk.cmake" # wasi-sdk < 25
cmake -S "$build/quickjs" -B "$build/quickjs/build" -G Ninja \
  -DQJS_BUILD_LIBC=OFF \
  -DQJS_BUILD_CLI_WITH_MIMALLOC=OFF \
  -DCMAKE_TOOLCHAIN_FILE="$toolchain" \
  -DCMAKE_PROJECT_INCLUDE=../qjswasm.cmake
ninja -C "$build/quickjs/build" qjswasm

out="$here/qjs.wasm"
# The one and only optimisation pass; the glue links with --no-wasm-opt so
# clang does not run its own when wasm-opt is on PATH.
wasm-opt -O3 "$build/quickjs/build/qjswasm" -o "$out"
rm -rf "$build"
ls -l "$out"
shasum -a 256 "$out"
