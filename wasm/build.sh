#!/usr/bin/env bash
# Rebuilds wasm/qjs.wasm: QuickJS-ng without quickjs-libc, with the
# fastschema/qjs glue from wasm/glue. See wasm/README.md.
#
# Needs: git, cmake, ninja, a wasi-sdk (WASI_SDK=/path/to/wasi-sdk-NN.N-...)
# and, for the committed size, wasm-opt (binaryen). Run from anywhere.
set -euo pipefail

QUICKJS_NG_COMMIT=d01ca4491fb24ccfeccb4c7394e28a3b21fd5986 # fastschema/qjs v0.0.6's submodule pin
: "${WASI_SDK:?set WASI_SDK to the wasi-sdk directory (its share/cmake holds wasi-sdk-p1.cmake)}"

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
if command -v wasm-opt >/dev/null; then
  wasm-opt -O3 "$build/quickjs/build/qjswasm" -o "$out"
else
  echo "wasm-opt not found: copying the unoptimised module (about 15% larger)" >&2
  cp "$build/quickjs/build/qjswasm" "$out"
fi
rm -rf "$build"
ls -l "$out"
shasum -a 256 "$out"
