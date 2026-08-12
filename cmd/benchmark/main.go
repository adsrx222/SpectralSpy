package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	SpectralSpy "github.com/adsrx222/SpectralSpy/src"
)

/*
go run cmd/benchmark/main.go \
  --output="./benchmark-results" \
  --dbpath="./live-demo/workspace/db.sqlite" \
  --waveform="./workspace/waveforms"
*/

func main() {
	outDir := flag.String("output", "", "Output directory for benchmark results")
	dbPath := flag.String("dbpath", "", "Path to the database")
	waveformPath := flag.String("waveform", "", "Path to directory containing WAV files")
	flag.Parse()

	if *outDir == "" {
		fmt.Println("Error: --output directory is required.")
		os.Exit(1)
	}

	if *dbPath == "" {
		fmt.Println("Error: --dbpath is required.")
		os.Exit(1)
	}

	if *waveformPath == "" {
		fmt.Println("Error: --waveform directory is required.")
		os.Exit(1)
	}

	absDir, err := filepath.Abs(*outDir)
	if err != nil {
		fmt.Printf("Error resolving output directory: %v\n", err)
		os.Exit(1)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05.000")
	runDir := filepath.Join(absDir, timestamp)

	if err := os.MkdirAll(runDir, 0755); err != nil {
		fmt.Printf("Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Benchmark results will be written to: %s\n", runDir)

	SpectralSpy.RunBenchmarks(runDir, *dbPath, *waveformPath)
}