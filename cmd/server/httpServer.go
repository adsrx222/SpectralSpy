package main

import (
	"log/slog"
	"os"

	"spectralspy/server"
)

func main() {
	if err := server.Run(); err != nil {
		slog.Error("Fatal server error", "err", err)
		os.Exit(1)
	}
}