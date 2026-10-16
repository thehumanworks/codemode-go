# The embedded QuickJS build

`qjs.wasm` is the WebAssembly module the sandbox runs. It is
[QuickJS-ng](https://github.com/quickjs-ng/quickjs) plus the C glue from
[fastschema/qjs](https://github.com/fastschema/qjs), compiled **without
quickjs-libc**. The stock module that ships inside the qjs Go package
includes libc, which gives scripts `qjs:std`, `qjs:os` and `qjs:bjson`
(files, environment, timers, `os.sleep`, `std.exit`, a file-backed module
loader) and a set of globals (`std`, `os`, `print`, `setTimeout`, ...).
Deleting the globals is easy; the module loader is C-level and cannot be
blocked from JavaScript, and a libc sleep cannot be interrupted by the
deadline. This build removes all of it, so there is nothing to reach and
nothing that can block.

The sandbox passes these bytes through `qjs.Option.QuickJSWasmBytes`; qjs
compiles whatever bytes it is given, process-wide, keyed by their hash.

## What changed against fastschema/qjs v0.0.6

`glue/` holds copies of the five C files and the CMake include from the
`qjswasm/` directory of fastschema/qjs v0.0.6 (MIT, see `glue/LICENSE`),
with these edits (`glue/upstream.diff` is the exact diff):

- `qjs.h`: no `quickjs-libc.h`.
- `qjs.c`: no `js_init_module_std/os/bjson`, no
  `js_std_set_worker_new_context_func`, no `js_std_init_handlers`. The
  module loader hook stays installed.
- `helpers.c`: no `js_std_add_helpers` and no init script that imported the
  libc modules into globals and defined the timers.
- `eval.c`: file input is rejected (no `js_load_file`), the JSON module
  loader is gone, `QJS_ModuleLoader` throws
  `ReferenceError: could not load module '<name>': this sandbox has no module loader`,
  `js_module_set_import_meta` is a stub, and `js_std_await`/`js_std_loop`
  (which the glue uses to settle top-level promises) are replaced by
  job-queue drivers: `js_std_await` raises an `InternalError` when the
  promise is pending and no jobs remain, instead of polling for I/O.
- `qjswasm.cmake`: `quickjs-libc.c` is no longer added to the target.

`gc` and `navigator` remain (they are qjs's, not libc's; the prelude deletes
`gc`). The exported symbol list is unchanged, so the qjs Go package drives
this module exactly as it drives the stock one.

## Pinned inputs

| Input | Version |
| --- | --- |
| quickjs-ng | commit `d01ca4491fb24ccfeccb4c7394e28a3b21fd5986` (the submodule pin of fastschema/qjs v0.0.6) |
| fastschema/qjs glue | v0.0.6 (`461716f4f380f81ffd09378751f1812919cddbca`), patched as above |
| wasi-sdk | 34.0 (clang 23.1.0, `wasi-sdk-p1.cmake`, target `wasm32-unknown-wasip1`) |
| binaryen | `wasm-opt` 133, `-O3` |

Flags are those of the upstream `Makefile` (`-DQJS_BUILD_CLI_WITH_MIMALLOC=OFF`,
`-O3`, LTO, `--stack-first`, `--initial-memory=1310720`) except
`-DQJS_BUILD_LIBC=OFF`.

## Rebuilding

```sh
WASI_SDK=/path/to/wasi-sdk-34.0-arm64-macos ./wasm/build.sh
```

The script clones quickjs-ng at the pinned commit into `wasm/build/`
(ignored by git), copies `glue/` next to it, configures with the wasi-sdk
toolchain file, builds the `qjswasm` target, optimises with `wasm-opt` when
present, writes `wasm/qjs.wasm` and prints its SHA-256. Run `go test ./...`
afterwards: `TestWasmHasNoLibc` and `TestSandboxIsolation` check the module
itself.

To move to a newer qjs release, re-apply `glue/upstream.diff` to its
`qjswasm/` directory, update the commit pins above, rebuild, and bump the
`github.com/fastschema/qjs` requirement together (the glue and the Go
package must match).
