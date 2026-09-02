# SpectralSpy

> Pure Golang implementation of an audio fingerprinting algorithm.

## Overview

SpectralSpy provides audio identification by generating spectral fingerprints from raw audio data. By leveraging time-frequency spectral analysis and adaptive thresholding, it converts audio signals into compact hash representations capable of matching audio snippets against a database with low latency and high accuracy.

### Key Features

- **Constant False Alarm Rate (CFAR) Filtering:** Employs adaptive CFAR thresholding during peak selection to increase fingerprint extraction robustness against background noise and signal interference.
- **Concurrency & Multithreading:** Utilizes Go native concurrency to parallelize processing, maximizing overall pipeline throughput while minimizing processing latency.
- **Distortion & Transcoding Resilient:** Tested and verified against severe audio degradations, including transcoding codecs, dynamic compression, parametric EQ adjustments, room reverberation, and variable bitrates.
- **Database & Storage Integration:** Native support for SQLite and D1 key-value storage for fast database lookups.

## Architecture

```text
SpectralSpy
├── benchmark-results
├── cmd
│   ├── benchmark
│   │   └── main.go
│   └── data
│       └── main.go
├── go.mod
├── go.sum
├── src
│   ├── benchmark.go
│   ├── data.go
│   ├── db.go
│   ├── fp-engine
│   │   └── fingerprint.go
│   ├── schema.sql
│   ├── server.go
│   ├── src_test.go
│   └── testutil.go
└── workspace
```

## Installation

This project is a Go package and should follow standard Go package installation and usage conventions.

Install the package with:

```bash
go get [github.com/adsrx222/SpectralSpy](https://github.com/adsrx222/SpectralSpy)
```

Import and use the package from your Go project:

```go
import "[github.com/adsrx222/SpectralSpy](https://github.com/adsrx222/SpectralSpy)"
```

For local development, clone the repository and use the standard Go tooling:

```bash
git clone [https://github.com/adsrx222/SpectralSpy.git](https://github.com/adsrx222/SpectralSpy.git)
cd SpectralSpy

go get .
```

## Usage

### Go Package Usage

Use the package through its exported Go APIs:

```go
package main

import (
    "context"
    "fmt"

    "[github.com/adsrx222/SpectralSpy](https://github.com/adsrx222/SpectralSpy)"
)

func main() {
    ctx := context.Background()
    fmt.Println("SpectralSpy initialized")
}
```

### Data Processing

To process data using the project's data-processing entry point:

```bash
go run ./path/to/sqlite_db.sqlite
```

Replace `./path/to/sqlite_db.sqlite` with the path to the SQLite database to be processed.

## Benchmarks

Run the benchmark suite with:

```bash
go run cmd/benchmark/main.go \
  --output="./path/to/benchmark-results" \
  --dbpath="./path/to/db/db.sqlite" \
  --waveform="./path/to/waveforms"
```

Benchmark sample data is supplemented by a directory of .WAV files specified by `--waveform`. 
Benchmark results should be written to the directory specified by `--output`.

### Benchmark Environment

| Property | Value |
|---|---|
| CPU | Apple M2 Pro |
| GPU | N/A |
| RAM | 16 GB |
| OS | macOS 26.5.2 |
| Go version | 1.25.1 |

### Performance Results

| Benchmark | Result | Notes |
|---|---:|---|
| Mean processing latency | 13.0 ms | Mean across 10 audio-duration benchmarks from 0.5–18.5 s |
| P95 latency | 24.6 ms | Mean P95 across 10 audio-duration benchmarks |
| P99 latency | 27.4 ms | Mean P99 across 10 audio-duration benchmarks |
| Peak hash rate | 4,596 hashes/s | Maximum measured hash rate at 4.5 s audio duration |
| Mean hash rate | 3,356 hashes/s | Mean across the 10 audio-duration benchmarks |

### Accuracy / Quality Results

| Metric | Result | Notes |
|---|---:|---|
| Reverb | 100% | 120/120 trials correct across low, moderate, high, and extreme reverberation |
| EQ | 100% | 120/120 trials correct across subtle, moderate, extreme, and surgical EQ |
| Compression | 100% | 90/90 trials correct across low, medium, and high compression |
| Bitrate | 100% | 150/150 trials correct across 256, 160, 96, 48, and 16 kbps |
| Noise | 100% | Correct-rate reported as 1.0 at the tested noise level |

### Robustness Results

#### Reverberation

| Condition | Hash Survival | Accuracy | Hash Rate |
|---|---:|---:|---:|
| Low Reverberation | 46.10% | 100% | 4,324.8 hashes/s |
| Moderate Reverberation | 31.13% | 100% | 3,165.7 hashes/s |
| High Reverberation | 19.36% | 100% | 3,534.4 hashes/s |
| Extreme Reverberation | 54.40% | 100% | 2,401.6 hashes/s |

#### EQ

| Condition | Hash Survival | Accuracy | Hash Rate |
|---|---:|---:|---:|
| Subtle | 95.60% | 100% | 5,749.8 hashes/s |
| Moderate | 92.48% | 100% | 4,763.5 hashes/s |
| Extreme | 89.43% | 100% | 4,621.6 hashes/s |
| Surgical | 89.01% | 100% | 5,825.2 hashes/s |

#### Compression

| Condition | Compression Ratio | Hash Survival | Accuracy | Hash Rate |
|---|---:|---:|---:|---:|
| Low Compression | 1.72:1 | 80.85% | 100% | 4,974.1 hashes/s |
| Medium Compression | 3.94:1 | 52.48% | 100% | 5,659.6 hashes/s |
| High Compression | 12.75:1 | 44.89% | 100% | 5,046.0 hashes/s |

#### Bitrate

| Bitrate | Hash Survival | Accuracy | Hash Rate |
|---:|---:|---:|---:|
| 256 kbps | 91.49% | 100% | 10,148.4 hashes/s |
| 160 kbps | 82.98% | 100% | 3,514.8 hashes/s |
| 96 kbps | 61.70% | 100% | 2,808.5 hashes/s |
| 48 kbps | 46.81% | 100% | 2,897.5 hashes/s |
| 16 kbps | 46.81% | 100% | 2,959.7 hashes/s |

### Processing Performance by Audio Duration

| Audio Duration | Mean Processing | P95 | P99 | Hash Count | Hash Rate |
|---:|---:|---:|---:|---:|---:|
| 0.5 s | 5.09 ms | 13.41 ms | 16.61 ms | 4 | 785.2 hashes/s |
| 2.5 s | 12.20 ms | 22.83 ms | 24.17 ms | 47 | 3,851.9 hashes/s |
| 4.5 s | 10.23 ms | 23.56 ms | 29.33 ms | 47 | 4,595.7 hashes/s |
| 6.5 s | 14.07 ms | 29.84 ms | 31.76 ms | 47 | 3,341.2 hashes/s |
| 8.5 s | 11.94 ms | 21.08 ms | 24.30 ms | 47 | 3,937.4 hashes/s |
| 10.5 s | 13.58 ms | 22.57 ms | 25.74 ms | 47 | 3,460.6 hashes/s |
| 12.5 s | 14.44 ms | 26.32 ms | 28.92 ms | 47 | 3,254.4 hashes/s |
| 14.5 s | 16.08 ms | 32.46 ms | 41.25 ms | 47 | 2,922.4 hashes/s |
| 16.5 s | 12.10 ms | 21.48 ms | 23.01 ms | 47 | 3,884.2 hashes/s |
| 18.5 s | 10.30 ms | 17.11 ms | 18.93 ms | 47 | 4,565.2 hashes/s |

### Benchmark Methodology

1. HTTP Workload Generation: Tested using Vegeta HTTP load testing against the POST /identify endpoint over a range of target rates (50–1,000 req/s).
2. Audio Sample Dataset: 100,089 sample queries extracted across a database of candidate tracks.
3. Signal Processing Evaluation: Benchmarked over 30 trial iterations per audio duration window (0.00s to 2.00s in 0.25s steps).
4. Robustness Transformations: Evaluated across synthetic perturbations covering bitrate drop, compression ratio, EQ filtering, and acoustic reverberation.

## Results Summary

**Performance:** Under sub-saturation conditions (≤200 req/s), P50 query latency remains beneath 3.5ms. Average audio signal extraction time for a 1.0-second query window is ~7.17ms.

**Accuracy:** Maintained a 100% correct track identification rate across all tested audio distortions including extreme reverberation, heavy compression, aggressive EQ notch filtering, and bitrate reductions down to 16 kbps.

**Scalability:** Handles throughput reliably up to ~435 req/s before queuing delays affect response latencies.

**Limitations:** Hash survival rates decrease under room reverberation (down to 16.86%) and dynamic range compression (down to 45.29%), though matching accuracy remains uncompromised due to offset alignment scoring.