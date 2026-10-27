#!/bin/sh
# Builds the wasm module and copies the Go runtime glue next to the web
# assets: wasm/index.html loads wasm_exec.js and nemo.wasm from this dir.
# Output: nemo.wasm, wasm_exec.js (both gitignored).
set -e
cd "$(dirname "$0")/.."
GOOS=js GOARCH=wasm go build -trimpath -o wasm/nemo.wasm ./wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" wasm/
echo "built wasm/nemo.wasm + wasm/wasm_exec.js"
