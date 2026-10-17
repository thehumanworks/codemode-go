{
  # Best effort: written without a Nix installation at hand. The first
  # `nix build` fails with a hash mismatch for the Go dependencies; copy the
  # hash Nix prints into vendorHash below and build again. The qjs-wasm
  # package compiles with nixpkgs' clang for wasm32-wasi rather than the
  # wasi-sdk release mise installs, so its bytes may differ from the
  # committed wasm/qjs.wasm while behaving the same.
  description = "codemode-go: sandboxed JavaScript that calls your Go functions, and a code tool for MCP servers";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # QuickJS-ng at the commit fastschema/qjs v0.0.6 pins; see wasm/README.md.
    quickjs-ng = {
      url = "github:quickjs-ng/quickjs/d01ca4491fb24ccfeccb4c7394e28a3b21fd5986";
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      quickjs-ng,
    }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.2.0";
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          inherit (pkgs) lib;
          # go.mod asks for Go 1.27; use that toolchain when the channel has it.
          buildGoModule = pkgs.buildGo127Module or pkgs.buildGoModule;

          # The library with its examples: builds every package, runs
          # `go test` for all of them, and installs the two example binaries.
          codemode-go = buildGoModule {
            pname = "codemode-go";
            inherit version;
            src = lib.cleanSource ./.;
            vendorHash = lib.fakeHash;
            ldflags = [
              "-s"
              "-w"
            ];
            meta = {
              description = "Sandboxed JavaScript code mode for Go tools and MCP servers";
              homepage = "https://github.com/thehumanworks/codemode-go";
              license = lib.licenses.mit;
            };
          };

          # wasm/qjs.wasm: QuickJS-ng without quickjs-libc, linked with the
          # patched fastschema/qjs glue in wasm/glue. Same flags as
          # wasm/build.sh, with nixpkgs' wasm32-wasi clang as the compiler.
          qjs-wasm = pkgs.pkgsCross.wasi32.stdenv.mkDerivation {
            pname = "qjs-wasm";
            inherit version;
            src = ./wasm/glue;
            nativeBuildInputs = [
              pkgs.cmake
              pkgs.ninja
              pkgs.binaryen
            ];
            dontUseCmakeConfigure = true;
            hardeningDisable = [ "all" ];
            # The glue expects the QuickJS-ng tree at ./quickjs next to it.
            postUnpack = ''
              cp -r ${quickjs-ng} "$sourceRoot/quickjs"
              chmod -R u+w "$sourceRoot/quickjs"
            '';
            buildPhase = ''
              runHook preBuild
              cmake -S quickjs -B build -G Ninja \
                -DCMAKE_SYSTEM_NAME=WASI \
                -DCMAKE_SYSTEM_PROCESSOR=wasm32 \
                -DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY \
                -DCMAKE_BUILD_TYPE=Release \
                -DQJS_BUILD_LIBC=OFF \
                -DQJS_BUILD_CLI_WITH_MIMALLOC=OFF \
                -DCMAKE_PROJECT_INCLUDE=../qjswasm.cmake
              ninja -C build qjswasm
              wasm-opt -O3 build/qjswasm -o qjs.wasm
              runHook postBuild
            '';
            installPhase = ''
              runHook preInstall
              install -Dm644 qjs.wasm "$out/qjs.wasm"
              runHook postInstall
            '';
            meta = {
              description = "QuickJS-ng for wasm32-wasi without quickjs-libc, for codemode-go";
              license = lib.licenses.mit;
            };
          };
        in
        {
          inherit codemode-go qjs-wasm;
          default = codemode-go;
        }
      );

      checks = forAllSystems (pkgs: {
        inherit (self.packages.${pkgs.stdenv.hostPlatform.system}) codemode-go qjs-wasm;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            (pkgs.go_1_27 or pkgs.go)
            pkgs.gopls
            pkgs.gotools
            # For the wasm rebuild. wasm/build.sh expects a wasi-sdk layout
            # (WASI_SDK/share/cmake/wasi-sdk-p1.cmake); under Nix use
            # `nix build .#qjs-wasm` and copy result/qjs.wasm to wasm/ instead.
            pkgs.cmake
            pkgs.ninja
            pkgs.binaryen
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
