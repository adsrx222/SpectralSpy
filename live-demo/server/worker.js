import './dist/wasm_exec.js';
import wasmModule from './dist/main.wasm';

const go = new Go();
let wasmInstance;

async function initWasm() {
  if (!wasmInstance) {
    const instance = await WebAssembly.instantiate(wasmModule, go.importObject);
    go.run(instance);
    wasmInstance = instance;
  }
}

export default {
  async fetch(request, env, ctx) {
    await initWasm();
    // Handles requests via the WASM runtime
    return globalThis.handleWorkerRequest(request, env);
  },
};