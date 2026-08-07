rm ./static/main.wasm
tinygo build -o ./static/main.wasm -target=wasm --no-debug ./wasm/wasm.go
go run -tags sqlite_math_functions ./cmd/server/httpServer.go