#!/usr/bin/env python3
import argparse
import os
import shutil
import subprocess
import sys

# python3 live-demo/client/scripts/build_wasm.py main.wasm live-demo/client/static live-demo/client/wasm/wasm.go

def compile_wasm(filename: str, dest_path: str, wasm_go_path: str):
    tinygo_bin = shutil.which("tinygo")
    if not tinygo_bin:
        print("Error: 'tinygo' executable not found in PATH.", file=sys.stderr)
        print("Please install TinyGo: https://tinygo.org/getting-started/", file=sys.stderr)
        sys.exit(1)

    if not os.path.isfile(wasm_go_path):
        print(f"Error: Go source file '{wasm_go_path}' does not exist.", file=sys.stderr)
        sys.exit(1)

    os.makedirs(dest_path, exist_ok=True)

    for item in os.listdir(dest_path):
        if item.endswith(".wasm"):
            file_to_remove = os.path.join(dest_path, item)
            if os.path.isfile(file_to_remove):
                os.remove(file_to_remove)
                print(f"Removed old WASM file: {file_to_remove}")

    output_filepath = os.path.join(dest_path, filename)

    cmd = [
        tinygo_bin,
        "build",
        "-target=wasm",
        "-o", output_filepath,
        wasm_go_path,
    ]

    print(f"Building WASM binary...")
    print(f"  Source:      {wasm_go_path}")
    print(f"  Destination: {output_filepath}")

    try:
        result = subprocess.run(cmd, check=True, capture_output=True, text=True)
        print("Compilation successful!")
        if result.stdout.strip():
            print(result.stdout)
    except subprocess.CalledProcessError as e:
        print("\nCompilation failed:", file=sys.stderr)
        print(e.stderr, file=sys.stderr)
        sys.exit(e.returncode)


def main():
    parser = argparse.ArgumentParser(
        description="Compile a Go source file to WebAssembly using TinyGo."
    )
    parser.add_argument(
        "filename",
        help="Name of output WASM file (e.g., main.wasm)"
    )
    parser.add_argument(
        "dest_path",
        help="Destination directory path (e.g., live-demo/client/public)"
    )
    parser.add_argument(
        "wasm_go_path",
        help="Path to WASM Go file (e.g., live-demo/client/cmd/main.go)"
    )

    args = parser.parse_args()

    compile_wasm(
        filename=args.filename,
        dest_path=args.dest_path,
        wasm_go_path=args.wasm_go_path,
    )


if __name__ == "__main__":
    main()