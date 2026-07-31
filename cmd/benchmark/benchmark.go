package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"spectralspy/pkg/SpectralSpy"
	"spectralspy/server"
	"spectralspy/testutil"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/mattn/go-sqlite3"
)

type BenchmarkSuite struct {
	App            *server.App
	DB             *sql.DB
	Metrics        []testutil.MetricReport
	Accuracies     []testutil.AccuracyResult
	BaseAudio      []float64
	ExpectedSongID string
	OutputPath     string
	BaseContext    context.Context
	Cancel         context.CancelFunc
}

func NewBenchmarkSuite(workspaceDir string) *BenchmarkSuite {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Hour)

	dbPath := filepath.Join(workspaceDir, "hashes.sqlite")
	dbConn := testutil.SetupTestDB(nil, dbPath)

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("mock_key", "mock_secret", "")),
	)
	if err != nil {
		cancel()
		panic(err)
	}
	dummyS3Client := s3.NewFromConfig(cfg)

	app := server.NewApp(dbConn, dummyS3Client, "bench-bucket", nil)
	app.DisableRateLimit = true

	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	path := filepath.Join("misc", "benchmarks-results", timestamp)
	if err := os.MkdirAll(path, 0755); err != nil {
		cancel()
		panic(err)
	}

	samples, songID, err := testutil.GetRandomMaestroSong(workspaceDir)
	if err != nil {
		cancel()
		panic(fmt.Errorf("failed to fetch benchmark sample: %w", err))
	}

	clampLen := int(10.0 * SpectralSpy.SAMPLE_RATE)
	if len(samples) > clampLen {
		samples = samples[:clampLen]
	}

	return &BenchmarkSuite{
		App:            app,
		DB:             dbConn,
		Metrics:        make([]testutil.MetricReport, 0),
		Accuracies:     make([]testutil.AccuracyResult, 0),
		BaseAudio:      samples,
		ExpectedSongID: songID,
		OutputPath:     path,
		BaseContext:    ctx,
		Cancel:         cancel,
	}
}

// 1. End-to-End Recognition Benchmark
func (b *BenchmarkSuite) RunEndToEndLatency() {
	var latencies []float64
	cropSamples := 5 * SpectralSpy.SAMPLE_RATE

	for i := 0; i < 100; i++ {
		// extract random 5-second continuous swath of audio before timer starts
		audioCrop := b.BaseAudio
		if len(b.BaseAudio) > cropSamples {
			maxStart := len(b.BaseAudio) - cropSamples
			startIdx := rand.Intn(maxStart + 1)
			audioCrop = b.BaseAudio[startIdx : startIdx+cropSamples]
		}

		// start measurement (excludes time spent cropping audio)
		start := time.Now()

		reqHashes := SpectralSpy.Process(b.BaseContext, audioCrop)

		fingerprints := make([]server.Fingerprint, len(reqHashes))
		for j, h := range reqHashes {
			fingerprints[j] = server.Fingerprint{Hash: h.Hash, AnchorTime: h.AnchorTime}
		}

		payload, _ := json.Marshal(server.IdentifyRequest{Fingerprints: fingerprints})

		req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(payload))
		req = req.WithContext(b.BaseContext)
		rr := httptest.NewRecorder()
		b.App.HandleIdentify(rr, req)

		dur := float64(time.Since(start).Milliseconds())
		latencies = append(latencies, dur)

		fmt.Printf("[%d / 100]  -> %.2f ms\n", i+1, dur)
	}

	b.Metrics = append(b.Metrics, testutil.MetricReport{
		Name:     "End-To-End Latency (ms)",
		Category: "Latency",
		Stats:    testutil.CalculateStats(latencies),
	})
}

// accuracy & robustness tests
func (b *BenchmarkSuite) measureAccuracy(name string, signal []float64) {
	fmt.Printf("  -> Measuring Accuracy for: %s\n", name)

	hashes := SpectralSpy.Process(b.BaseContext, signal)
	if len(hashes) == 0 {
		b.Accuracies = append(b.Accuracies, testutil.AccuracyResult{Category: name, Top1: 0.0, FalseNeg: 1.0})
		return
	}

	fingerprints := make([]server.Fingerprint, len(hashes))
	for j, h := range hashes {
		fingerprints[j] = server.Fingerprint{Hash: h.Hash, AnchorTime: h.AnchorTime}
	}

	// extract a random continuous swath of max 500 fingerprints
	fingerprints = testutil.GetContinuousSwath(fingerprints, server.MAX_REQUEST_LENGTH)

	payload, _ := json.Marshal(server.IdentifyRequest{Fingerprints: fingerprints})
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(payload))
	req = req.WithContext(b.BaseContext)
	rr := httptest.NewRecorder()
	b.App.HandleIdentify(rr, req)

	var resp server.IdentifyResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	score := 0.0
	fn := 1.0
	if resp.SongID == b.ExpectedSongID {
		score = 100.0
		fn = 0.0
	}

	b.Accuracies = append(b.Accuracies, testutil.AccuracyResult{
		Category:  name,
		Top1:      score,
		Precision: score,
		Recall:    score,
		F1Score:   score,
		FalseNeg:  fn,
	})
}

func (b *BenchmarkSuite) RunRobustnessSuite() {
	for _, snr := range []float64{30, 20, 15, 10, 5, 0} {
		noisy := testutil.AddNoise(b.BaseAudio, snr)
		b.measureAccuracy(fmt.Sprintf("Noise %ddB SNR", int(snr)), noisy)
	}

	b.measureAccuracy("Compression (Simulated 128kbps)", testutil.SimulateCompression(b.BaseAudio, 8000.0))
	b.measureAccuracy("Compression (Simulated 64kbps)", testutil.SimulateCompression(b.BaseAudio, 4000.0))

	for _, speed := range []float64{0.95, 0.98, 1.02, 1.05} {
		b.measureAccuracy(fmt.Sprintf("Speed %.2fx", speed), testutil.ResampleAudio(b.BaseAudio, speed))
	}

	for _, length := range []int{1, 2, 3, 5} {
		samples := length * SpectralSpy.SAMPLE_RATE
		if samples > len(b.BaseAudio) {
			continue
		}
		b.measureAccuracy(fmt.Sprintf("Clip Length %ds", length), b.BaseAudio[:samples])
	}
}

// fingerprint feature quality
func (b *BenchmarkSuite) RunFingerprintQuality() {
	fmt.Println("  -> Analyzing Feature Quality...")
	hashes := SpectralSpy.Process(b.BaseContext, b.BaseAudio)
	counts := make(map[uint64]int)
	for _, h := range hashes {
		counts[h.Hash]++
	}

	var entropy float64
	total := float64(len(hashes))
	for _, count := range counts {
		prob := float64(count) / total
		entropy -= prob * math.Log2(prob)
	}

	collisions := float64(len(hashes) - len(counts))

	b.Metrics = append(b.Metrics, testutil.MetricReport{
		Name:     "Hash Entropy (bits)",
		Category: "Quality",
		Stats:    testutil.Stats{Mean: entropy},
	})
	b.Metrics = append(b.Metrics, testutil.MetricReport{
		Name:     "Hash Collisions",
		Category: "Quality",
		Stats:    testutil.Stats{Mean: collisions},
	})
}

// database and api scalability
func (b *BenchmarkSuite) RunAPILoadTest() {
	users := []int{1, 10, 100, 500}

	payload := `{"fingerprints": [{"hash": 101, "anchor_time": 0.0}]}`

	for _, concurrent := range users {
		fmt.Printf("  -> Testing Load Concurrency: %d Users...\n", concurrent)

		var latencies []float64
		var mu sync.Mutex
		var wg sync.WaitGroup

		for i := 0; i < concurrent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()

				req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader([]byte(payload)))
				req.RemoteAddr = fmt.Sprintf("192.168.1.%d:80", rand.Intn(250))
				req = req.WithContext(b.BaseContext)

				rr := httptest.NewRecorder()
				b.App.HandleIdentify(rr, req)

				dur := float64(time.Since(start).Milliseconds())
				mu.Lock()
				latencies = append(latencies, dur)
				mu.Unlock()
			}()
		}
		wg.Wait()

		b.Metrics = append(b.Metrics, testutil.MetricReport{
			Name:     fmt.Sprintf("API Latency (%d users) ms", concurrent),
			Category: "API Load",
			Stats:    testutil.CalculateStats(latencies),
		})
	}
}

// Report Generation
func (b *BenchmarkSuite) GenerateReports() {
	jsonFile, _ := os.Create(filepath.Join(b.OutputPath, "benchmark_results.json"))
	json.NewEncoder(jsonFile).Encode(map[string]interface{}{
		"metrics":    b.Metrics,
		"accuracies": b.Accuracies,
	})
	jsonFile.Close()

	csvFile, _ := os.Create(filepath.Join(b.OutputPath, "latency.csv"))
	writer := csv.NewWriter(csvFile)
	writer.Write([]string{"Benchmark", "Category", "Count", "Mean", "P50", "P90", "P95", "P99", "StdDev"})
	for _, m := range b.Metrics {
		writer.Write([]string{
			m.Name, m.Category, strconv.Itoa(m.Stats.Count),
			fmt.Sprintf("%.2f", m.Stats.Mean), fmt.Sprintf("%.2f", m.Stats.P50),
			fmt.Sprintf("%.2f", m.Stats.P90), fmt.Sprintf("%.2f", m.Stats.P95),
			fmt.Sprintf("%.2f", m.Stats.P99), fmt.Sprintf("%.2f", m.Stats.StdDev),
		})
	}
	writer.Flush()
	csvFile.Close()

	accFile, _ := os.Create(filepath.Join(b.OutputPath, "accuracy.csv"))
	accWriter := csv.NewWriter(accFile)
	accWriter.Write([]string{"Category", "Top-1", "Precision", "Recall", "F1", "FalseNeg"})
	for _, a := range b.Accuracies {
		accWriter.Write([]string{
			a.Category, fmt.Sprintf("%.2f", a.Top1),
			fmt.Sprintf("%.2f", a.Precision), fmt.Sprintf("%.2f", a.Recall),
			fmt.Sprintf("%.2f", a.F1Score), fmt.Sprintf("%.2f", a.FalseNeg),
		})
	}
	accWriter.Flush()
	accFile.Close()

	sysFile, _ := os.Create(filepath.Join(b.OutputPath, "system_information.json"))
	json.NewEncoder(sysFile).Encode(map[string]string{
		"os":           runtime.GOOS,
		"architecture": runtime.GOARCH,
		"cpus":         strconv.Itoa(runtime.NumCPU()),
		"go_version":   runtime.Version(),
	})
	sysFile.Close()

	mdFile, _ := os.Create(filepath.Join(b.OutputPath, "benchmark_report.md"))
	fmt.Fprintf(mdFile, "# Audio Fingerprint Benchmark Report\n\n")
	fmt.Fprintf(mdFile, "## Executive Summary\nAutomated CI benchmark suite executing accuracy, database, and concurrency workloads.\n\n")

	fmt.Fprintf(mdFile, "## Latency Metrics\n| Stage | Mean | P95 | P99 |\n|---|---|---|---|\n")
	for _, m := range b.Metrics {
		fmt.Fprintf(mdFile, "| %s | %.2f | %.2f | %.2f |\n", m.Name, m.Stats.Mean, m.Stats.P95, m.Stats.P99)
	}

	fmt.Fprintf(mdFile, "\n## Accuracy\n| Target | Top-1 | F1 Score |\n|---|---|---|\n")
	for _, a := range b.Accuracies {
		fmt.Fprintf(mdFile, "| %s | %.1f%% | %.2f |\n", a.Category, a.Top1, a.F1Score)
	}
	mdFile.Close()
}

func main() {

	// --- Live Diagnostic Server ---
	go func() {
		fmt.Println("Live diagnostics running at http://localhost:6060/debug/pprof/")
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			fmt.Printf("pprof server error: %v\n", err)
		}
	}()

	// Enable detailed block and mutex tracking
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	// ------------------------------

	workspaceDir := filepath.Join("misc", "workspace")

	fmt.Println("Initializing Benchmark Suite...")
	suite := NewBenchmarkSuite(workspaceDir)
	defer suite.Cancel()

	fmt.Println("\nStarting End-to-End Latency Tests...")
	suite.RunEndToEndLatency()

	fmt.Println("\nStarting Robustness & Accuracy Tests...")
	suite.RunRobustnessSuite()

	fmt.Println("\nEvaluating Fingerprint Hash Entropy...")
	suite.RunFingerprintQuality()

	fmt.Println("\nStress Testing API Concurrency...")
	suite.RunAPILoadTest()

	fmt.Println("\nGenerating Reports...")
	suite.GenerateReports()

	fmt.Printf("\nBenchmark complete. Results exported to %s\n", suite.OutputPath)
}