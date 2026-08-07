package main

import (
	"log/slog"
	"os"

	"github.com/adsrx222/SpectralSpy/server"
)

func main() {
	if err := server.Run(); err != nil {
		slog.Error("Fatal server error", "err", err)
		os.Exit(1)
	}
}