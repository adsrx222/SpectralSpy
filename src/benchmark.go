package SpectralSpy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"time"

	SpectralSpyEngine "github.com/adsrx222/SpectralSpy/src/fp-engine"
	"github.com/gin-gonic/gin"
	"github.com/tsenart/vegeta/v12/lib"
)

func RunBenchmarks(runDir, dbPath, waveformPath string) {
	fmt.Println("Starting Benchmarks...")

	// create random 2-second clean WAV clip.
	cleanWavPath, err := findRandomPath(waveformPath, 2)
	if err != nil {
		fmt.Printf("Error creating clean wav: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(cleanWavPath)

	// load the clean WAV samples.
	cleanSamples, err := processWAV(cleanWavPath)
	if err != nil {
		fmt.Printf("Error loading clean wav: %v\n", err)
		os.Exit(1)
	}

	cleanFps := SpectralSpyEngine.Process(context.Background(), cleanSamples)

	dbMap := make(map[uint64][]SpectralSpyEngine.DBEntry)
	for _, fp := range cleanFps {
		dbMap[fp.Hash] = append(dbMap[fp.Hash], SpectralSpyEngine.DBEntry{
			Hash:       fp.Hash,
			SongID:     "test-song",
			AnchorTime: fp.AnchorTime,
			Weight:     1.0,
		})
	}

	// write run metadata
	meta := map[string]interface{}{
		"timestamp":        time.Now().Format(time.RFC3339),
		"input_audio":      cleanWavPath,
		"duration_seconds": 2.0,
		"num_clean_hashes": len(cleanFps),
	}
	writeJSON(filepath.Join(runDir, "run_metadata.json"), meta)

	BM1_SNR(runDir, cleanSamples, cleanFps, dbMap)
	BM2_TRANSCODE(runDir, cleanWavPath, cleanFps, dbMap)
	BM3_COMPRESSION(runDir, cleanWavPath, cleanFps, dbMap)
	BM4_EQ(runDir, cleanWavPath, cleanFps, dbMap)
	BM5_REVERB(runDir, cleanWavPath, cleanFps, dbMap)
	BM6_HASHRATE(runDir, cleanSamples)
	BM7_HASHCOLLISION(runDir, dbPath)
	BM8_LOADTEST(runDir, dbPath, cleanFps)

	fmt.Println("Benchmarks Complete!")
}

func BM1_SNR(runDir string, cleanSamples []float64, cleanFps []SpectralSpyEngine.Fingerprint, dbMap map[uint64][]SpectralSpyEngine.DBEntry) {
	fmt.Println("Running Benchmark #1: Noise")

	type NoiseResult struct {
		NoiseLevel           float64 `json:"noise_level"`
		HashSurvival         float64 `json:"hash_survival"`
		AccuracyCorrectRate  float64 `json:"accuracy_correct_rate"`
		AccuracyOffsetMargin float64 `json:"accuracy_offset_margin"`
		HashRate             float64 `json:"hash_rate"`
	}
	var results []NoiseResult

	rng := rand.New(rand.NewSource(42))
	noiseLevel := 0.0
	noiseIncrement := 0.005
	trials := 10

	cleanSet := make(map[uint64]struct{})
	for _, fp := range cleanFps {
		cleanSet[fp.Hash] = struct{}{}
	}

	// run & increment noise amplitude until 50% hash survival rate
	for {
		var survivalStats []float64
		var hrStats []float64
		var offsetStats []float64
		correctCount := 0

		for t := 0; t < trials; t++ {
			noisySamples := addNoise(cleanSamples, noiseLevel, rng)

			t0 := time.Now()
			noisyFps := SpectralSpyEngine.Process(context.Background(), noisySamples)
			dur := time.Since(t0)

			hr := 0.0
			if dur.Seconds() > 0 {
				hr = float64(len(noisyFps)) / dur.Seconds()
			}
			hrStats = append(hrStats, hr)

			survived := 0
			for _, fp := range noisyFps {
				if _, ok := cleanSet[fp.Hash]; ok {
					survived++
				}
			}
			survivalStats = append(survivalStats, float64(survived)/float64(len(cleanFps)))

			bestSongID, _, timeOffset, _ := SpectralSpyEngine.MatchFingerprints(noisyFps, dbMap)
			if bestSongID == "test-song" {
				correctCount++
			}
			offsetStats = append(offsetStats, timeOffset)
		}

		avgSurvival := avg(survivalStats)

		results = append(results, NoiseResult{
			NoiseLevel:           noiseLevel,
			HashSurvival:         avgSurvival,
			AccuracyCorrectRate:  float64(correctCount) / float64(trials),
			AccuracyOffsetMargin: avg(offsetStats),
			HashRate:             avg(hrStats),
		})

		if avgSurvival < 0.5 {
			break
		}
		noiseLevel += noiseIncrement
	}
	writeJSON(filepath.Join(runDir, "BM1_SNR.json"), results)
}

func BM2_TRANSCODE(runDir string, cleanWavPath string, cleanFps []SpectralSpyEngine.Fingerprint, dbMap map[uint64][]SpectralSpyEngine.DBEntry) {
	fmt.Println("Running Benchmark #2: Bitrate")
	bitrates := []int{256, 160, 96, 48, 16}
	trials := 30

	type BitrateResult struct {
		Bitrate              int                `json:"bitrate"`
		NumberOfTrials       int                `json:"number_of_trials"`
		HashSurvival         float64            `json:"hash_survival"`
		AccuracyCorrectRate  float64            `json:"accuracy_correct_rate"`
		AccuracyOffsetMargin float64            `json:"accuracy_offset_margin"`
		HashRate             float64            `json:"hash_rate"`
		AggregateStats       map[string]float64 `json:"aggregate_statistics"`
	}
	var allResults []BitrateResult

	cleanSet := make(map[uint64]struct{})
	for _, fp := range cleanFps {
		cleanSet[fp.Hash] = struct{}{}
	}

	for _, br := range bitrates {
		var survivalStats []float64
		var hrStats []float64
		var offsetStats []float64
		correctCount := 0

		for t := 0; t < trials; t++ {
			samples, err := runFFmpegTranscode(cleanWavPath, "mp3", fmt.Sprintf("%dk", br))
			if err != nil {
				fmt.Printf("ffmpeg failed for bitrate %d: %v\n", br, err)
				os.Exit(1)
			}

			t0 := time.Now()
			fps := SpectralSpyEngine.Process(context.Background(), samples)
			dur := time.Since(t0)

			hr := 0.0
			if dur.Seconds() > 0 {
				hr = float64(len(fps)) / dur.Seconds()
			}
			hrStats = append(hrStats, hr)

			survived := 0
			for _, fp := range fps {
				if _, ok := cleanSet[fp.Hash]; ok {
					survived++
				}
			}
			surv := float64(survived) / float64(len(cleanFps))
			survivalStats = append(survivalStats, surv)

			bestSongID, _, timeOffset, _ := SpectralSpyEngine.MatchFingerprints(fps, dbMap)
			if bestSongID == "test-song" {
				correctCount++
			}
			offsetStats = append(offsetStats, timeOffset)
		}

		allResults = append(allResults, BitrateResult{
			Bitrate:              br,
			NumberOfTrials:       trials,
			HashSurvival:         avg(survivalStats),
			AccuracyCorrectRate:  float64(correctCount) / float64(trials),
			AccuracyOffsetMargin: avg(offsetStats),
			HashRate:             avg(hrStats),
			AggregateStats: map[string]float64{
				"survival_min": getMin(survivalStats),
				"survival_max": getMax(survivalStats),
				"hashrate_min": getMin(hrStats),
				"hashrate_max": getMax(hrStats),
			},
		})
	}
	writeJSON(filepath.Join(runDir, "BM2_TRANSCODE.json"), allResults)
}

func BM3_COMPRESSION(runDir string, cleanWavPath string, cleanFps []SpectralSpyEngine.Fingerprint, dbMap map[uint64][]SpectralSpyEngine.DBEntry) {
	fmt.Println("Running Benchmark #3: Dynamic Compression")
	rng := rand.New(rand.NewSource(123))
	trials := 30

	type CompResult struct {
		CompressionCategory  string             `json:"compression_category"`
		Ratio                float64            `json:"ratio"`
		Threshold            float64            `json:"threshold"`
		NumberOfTrials       int                `json:"number_of_trials"`
		HashSurvival         float64            `json:"hash_survival"`
		AccuracyCorrectRate  float64            `json:"accuracy_correct_rate"`
		AccuracyOffsetMargin float64            `json:"accuracy_offset_margin"`
		HashRate             float64            `json:"hash_rate"`
		AggregateStats       map[string]float64 `json:"aggregate_statistics"`
	}

	categories := []struct {
		Name       string
		MinR, MaxR float64
		MinT, MaxT float64
	}{
		{"Low Compression", 1.5, 2.0, -18.0, -12.0},
		{"Medium Compression", 3.0, 5.0, -24.0, -18.0},
		{"High Compression", 8.0, 20.0, -40.0, -24.0},
	}

	cleanSet := make(map[uint64]struct{})
	for _, fp := range cleanFps {
		cleanSet[fp.Hash] = struct{}{}
	}

	var allResults []CompResult
	for _, cat := range categories {
		var survivalStats []float64
		var hrStats []float64
		var offsetStats []float64
		correctCount := 0
		var avgRatio, avgThresh float64

		for t := 0; t < trials; t++ {
			ratio := cat.MinR + rng.Float64()*(cat.MaxR-cat.MinR)
			thresh := cat.MinT + rng.Float64()*(cat.MaxT-cat.MinT)
			avgRatio += ratio
			avgThresh += thresh

			filter := fmt.Sprintf("acompressor=ratio=%f:threshold=%fdB", ratio, thresh)
			samples, err := runFFmpegFilter(cleanWavPath, filter)
			if err != nil {
				fmt.Printf("ffmpeg failed for comp filter %s: %v\n", filter, err)
				os.Exit(1)
			}

			t0 := time.Now()
			fps := SpectralSpyEngine.Process(context.Background(), samples)
			dur := time.Since(t0)

			hr := 0.0
			if dur.Seconds() > 0 {
				hr = float64(len(fps)) / dur.Seconds()
			}
			hrStats = append(hrStats, hr)

			survived := 0
			for _, fp := range fps {
				if _, ok := cleanSet[fp.Hash]; ok {
					survived++
				}
			}
			survivalStats = append(survivalStats, float64(survived)/float64(len(cleanFps)))

			bestSongID, _, timeOffset, _ := SpectralSpyEngine.MatchFingerprints(fps, dbMap)
			if bestSongID == "test-song" {
				correctCount++
			}
			offsetStats = append(offsetStats, timeOffset)
		}

		allResults = append(allResults, CompResult{
			CompressionCategory:  cat.Name,
			Ratio:                avgRatio / float64(trials),
			Threshold:            avgThresh / float64(trials),
			NumberOfTrials:       trials,
			HashSurvival:         avg(survivalStats),
			AccuracyCorrectRate:  float64(correctCount) / float64(trials),
			AccuracyOffsetMargin: avg(offsetStats),
			HashRate:             avg(hrStats),
			AggregateStats: map[string]float64{
				"survival_min": getMin(survivalStats),
				"survival_max": getMax(survivalStats),
			},
		})
	}
	writeJSON(filepath.Join(runDir, "BM3_COMPRESSION.json"), allResults)
}

func BM4_EQ(runDir string, cleanWavPath string, cleanFps []SpectralSpyEngine.Fingerprint, dbMap map[uint64][]SpectralSpyEngine.DBEntry) {
	fmt.Println("Running Benchmark #4: Equalization")
	rng := rand.New(rand.NewSource(124))
	trials := 30

	type EQResult struct {
		EQCategory           string             `json:"eq_category"`
		Gain                 float64            `json:"gain"`
		QWidth               float64            `json:"q_width"`
		NumberOfTrials       int                `json:"number_of_trials"`
		HashSurvival         float64            `json:"hash_survival"`
		AccuracyCorrectRate  float64            `json:"accuracy_correct_rate"`
		AccuracyOffsetMargin float64            `json:"accuracy_offset_margin"`
		HashRate             float64            `json:"hash_rate"`
		AggregateStats       map[string]float64 `json:"aggregate_statistics"`
	}

	categories := []struct {
		Name       string
		MinG, MaxG float64
		Width      float64
	}{
		{"Subtle", -3.0, 3.0, 0.7},
		{"Moderate", -6.0, 6.0, 1.4},
		{"Extreme", -15.0, 15.0, 3.0},
		{"Surgical", -30.0, -20.0, 10.0},
	}

	cleanSet := make(map[uint64]struct{})
	for _, fp := range cleanFps {
		cleanSet[fp.Hash] = struct{}{}
	}

	var allResults []EQResult
	for _, cat := range categories {
		var survivalStats []float64
		var hrStats []float64
		var offsetStats []float64
		correctCount := 0
		var avgGain float64

		for t := 0; t < trials; t++ {
			gain := cat.MinG + rng.Float64()*(cat.MaxG-cat.MinG)
			avgGain += gain
			freq := 200.0 + rng.Float64()*4000.0 // random freq

			filter := fmt.Sprintf("equalizer=f=%f:width_type=q:width=%f:g=%f", freq, cat.Width, gain)
			samples, err := runFFmpegFilter(cleanWavPath, filter)
			if err != nil {
				fmt.Printf("ffmpeg failed for eq filter %s: %v\n", filter, err)
				os.Exit(1)
			}

			t0 := time.Now()
			fps := SpectralSpyEngine.Process(context.Background(), samples)
			dur := time.Since(t0)
			hr := 0.0
			if dur.Seconds() > 0 {
				hr = float64(len(fps)) / dur.Seconds()
			}
			hrStats = append(hrStats, hr)

			survived := 0
			for _, fp := range fps {
				if _, ok := cleanSet[fp.Hash]; ok {
					survived++
				}
			}
			survivalStats = append(survivalStats, float64(survived)/float64(len(cleanFps)))

			bestSongID, _, timeOffset, _ := SpectralSpyEngine.MatchFingerprints(fps, dbMap)
			if bestSongID == "test-song" {
				correctCount++
			}
			offsetStats = append(offsetStats, timeOffset)
		}

		allResults = append(allResults, EQResult{
			EQCategory:           cat.Name,
			Gain:                 avgGain / float64(trials),
			QWidth:               cat.Width,
			NumberOfTrials:       trials,
			HashSurvival:         avg(survivalStats),
			AccuracyCorrectRate:  float64(correctCount) / float64(trials),
			AccuracyOffsetMargin: avg(offsetStats),
			HashRate:             avg(hrStats),
			AggregateStats: map[string]float64{
				"survival_min": getMin(survivalStats),
				"survival_max": getMax(survivalStats),
			},
		})
	}
	writeJSON(filepath.Join(runDir, "BM4_EQ.json"), allResults)
}

func BM5_REVERB(runDir string, cleanWavPath string, cleanFps []SpectralSpyEngine.Fingerprint, dbMap map[uint64][]SpectralSpyEngine.DBEntry) {
	fmt.Println("Running Benchmark #5: Reverberation")
	rng := rand.New(rand.NewSource(125))
	trials := 30

	type ReverbResult struct {
		ReverbCategory       string             `json:"reverb_category"`
		RoomSize             float64            `json:"room_size"`
		WetMix               float64            `json:"wet_mix"`
		NumberOfTrials       int                `json:"number_of_trials"`
		HashSurvival         float64            `json:"hash_survival"`
		AccuracyCorrectRate  float64            `json:"accuracy_correct_rate"`
		AccuracyOffsetMargin float64            `json:"accuracy_offset_margin"`
		HashRate             float64            `json:"hash_rate"`
		AggregateStats       map[string]float64 `json:"aggregate_statistics"`
	}

	categories := []struct {
		Name             string
		MinRoom, MaxRoom float64
		MinWet, MaxWet   float64
	}{
		{"Low Reverberation", 0.10, 0.25, 0.05, 0.15},
		{"Moderate Reverberation", 0.40, 0.60, 0.20, 0.40},
		{"High Reverberation", 0.75, 0.90, 0.45, 0.65},
		{"Extreme Reverberation", 0.95, 0.99, 0.70, 0.95},
	}

	cleanSet := make(map[uint64]struct{})
	for _, fp := range cleanFps {
		cleanSet[fp.Hash] = struct{}{}
	}

	var allResults []ReverbResult
	for _, cat := range categories {
		var survivalStats []float64
		var hrStats []float64
		var offsetStats []float64
		correctCount := 0
		var avgRoom, avgWet float64

		for t := 0; t < trials; t++ {
			roomSize := cat.MinRoom + rng.Float64()*(cat.MaxRoom-cat.MinRoom)
			wetMix := cat.MinWet + rng.Float64()*(cat.MaxWet-cat.MinWet)
			avgRoom += roomSize
			avgWet += wetMix

			inGain := 1.0 - wetMix
			delay := roomSize * 100.0 // ms
			if delay < 1.0 {
				delay = 1.0
			}
			decay := roomSize * 0.8
			filter := fmt.Sprintf("aecho=in_gain=%f:out_gain=%f:delays=%f:decays=%f", inGain, wetMix, delay, decay)

			samples, err := runFFmpegFilter(cleanWavPath, filter)
			if err != nil {
				fmt.Printf("ffmpeg failed for reverb filter %s: %v\n", filter, err)
				os.Exit(1)
			}

			t0 := time.Now()
			fps := SpectralSpyEngine.Process(context.Background(), samples)
			dur := time.Since(t0)
			hr := 0.0
			if dur.Seconds() > 0 {
				hr = float64(len(fps)) / dur.Seconds()
			}
			hrStats = append(hrStats, hr)

			survived := 0
			for _, fp := range fps {
				if _, ok := cleanSet[fp.Hash]; ok {
					survived++
				}
			}
			survivalStats = append(survivalStats, float64(survived)/float64(len(cleanFps)))

			bestSongID, _, timeOffset, _ := SpectralSpyEngine.MatchFingerprints(fps, dbMap)
			if bestSongID == "test-song" {
				correctCount++
			}
			offsetStats = append(offsetStats, timeOffset)
		}

		allResults = append(allResults, ReverbResult{
			ReverbCategory:       cat.Name,
			RoomSize:             avgRoom / float64(trials),
			WetMix:               avgWet / float64(trials),
			NumberOfTrials:       trials,
			HashSurvival:         avg(survivalStats),
			AccuracyCorrectRate:  float64(correctCount) / float64(trials),
			AccuracyOffsetMargin: avg(offsetStats),
			HashRate:             avg(hrStats),
			AggregateStats: map[string]float64{
				"survival_min": getMin(survivalStats),
				"survival_max": getMax(survivalStats),
			},
		})
	}
	writeJSON(filepath.Join(runDir, "BM5_REVERB.json"), allResults)
}

func BM6_HASHRATE(runDir string, cleanSamples []float64) {
	fmt.Println("Running Benchmark #6: Hash Rate")
	trials := 30

	type HashRateResult struct {
		AudioDuration          float64 `json:"audio_duration"`
		TrialCount             int     `json:"trial_count"`
		MeanProcessingDuration float64 `json:"mean_processing_duration"`
		P95ProcessingDuration  float64 `json:"p95_processing_duration"`
		P99ProcessingDuration  float64 `json:"p99_processing_duration"`
		HashCount              int     `json:"hash_count"`
		HashesPerSecond        float64 `json:"hashes_per_second"`
	}

	var allResults []HashRateResult

	for durSec := 0.5; durSec <= 20.0; durSec += 2.0 {
		length := int(durSec * SpectralSpyEngine.SAMPLE_RATE)
		if length > len(cleanSamples) {
			length = len(cleanSamples)
		}
		slice := cleanSamples[:length]

		var durations []float64
		var hashCount int

		for t := 0; t < trials; t++ {
			t0 := time.Now()
			fps := SpectralSpyEngine.Process(context.Background(), slice)
			durations = append(durations, time.Since(t0).Seconds())
			hashCount = len(fps)
		}

		sort.Float64s(durations)
		p95 := percentile(durations, 95)
		p99 := percentile(durations, 99)
		mean := avg(durations)

		hr := 0.0
		if mean > 0 {
			hr = float64(hashCount) / mean
		}

		allResults = append(allResults, HashRateResult{
			AudioDuration:          durSec,
			TrialCount:             trials,
			MeanProcessingDuration: mean,
			P95ProcessingDuration:  p95,
			P99ProcessingDuration:  p99,
			HashCount:              hashCount,
			HashesPerSecond:        hr,
		})
	}
	writeJSON(filepath.Join(runDir, "BM6_HASHRATE.json"), allResults)
}

func BM7_HASHCOLLISION(runDir, dbPath string) {
	fmt.Println("Running Benchmark #7: Hash Collision")

	dbConn, err := getDBConn(dbPath)
	if err != nil {
		fmt.Printf("db setup failed: %v\n", err)
		return
	}

	rows, err := dbConn.Query("SELECT track_count FROM hash_weight")
	if err != nil {
		fmt.Printf("db query failed: %v\n", err)
		return
	}
	defer rows.Close()

	var trackCounts []float64
	for rows.Next() {
		var tc float64
		if err := rows.Scan(&tc); err == nil {
			trackCounts = append(trackCounts, tc)
		}
	}

	if len(trackCounts) == 0 {
		fmt.Println("No rows found for collision benchmark")
		return
	}

	sort.Float64s(trackCounts)
	p50 := percentile(trackCounts, 50)
	p95 := percentile(trackCounts, 95)
	p99 := percentile(trackCounts, 99)
	p100 := trackCounts[len(trackCounts)-1]

	res := map[string]interface{}{
		"sample_count":     len(trackCounts),
		"p50_track_count":  p50,
		"p95_track_count":  p95,
		"p99_track_count":  p99,
		"p100_track_count": p100,
	}
	writeJSON(filepath.Join(runDir, "BM7_HASHCOLLISION.json"), res)
}

func BM8_LOADTEST(runDir, dbPath string, cleanFps []SpectralSpyEngine.Fingerprint) {
	fmt.Println("Running Benchmark #8: Full HTTP Load Testing")

	dbConn, err := getDBConn(dbPath)
	if err != nil {
		fmt.Printf("db setup failed: %v\n", err)
		return
	}

	rates := []int{50, 100, 200, 500, 1000}

	router := gin.New()
	router.POST("/identify", NewIdentifyHandler(dbConn, nil))
	ts := httptest.NewServer(router)
	defer ts.Close()

	reqBody, _ := json.Marshal(IdentifyRequest{Fingerprints: cleanFps})
	targeter := vegeta.NewStaticTargeter(vegeta.Target{
		Method: "POST",
		URL:    ts.URL + "/identify",
		Body:   reqBody,
	})

	duration := 5 * time.Second

	results := make([]map[string]interface{}, 0, len(rates))

	for _, rate := range rates {
		fmt.Printf("Running load test at %d req/s for %s...\n", rate, duration)

		attacker := vegeta.NewAttacker()
		vegetaRate := vegeta.Rate{
			Freq: rate,
			Per:  time.Second,
		}

		var metrics vegeta.Metrics
		for res := range attacker.Attack(
			targeter,
			vegetaRate,
			duration,
			fmt.Sprintf("Load Test %d req/s", rate),
		) {
			metrics.Add(res)
		}
		metrics.Close()

		res := map[string]interface{}{
			"test_configuration": fmt.Sprintf("Vegeta %dreq/s for %s", rate, duration),
			"request_rate":       float64(rate),
			"p50":                metrics.Latencies.P50.Seconds(),
			"p95":                metrics.Latencies.P95.Seconds(),
			"p99":                metrics.Latencies.P99.Seconds(),
			"throughput":         metrics.Throughput,
			"error_rate":         metrics.Errors,
			"test_duration":      metrics.Duration.Seconds(),
			"target_endpoint":    "POST /identify",
		}

		results = append(results, res)
	}

	res := map[string]interface{}{
		"test_configuration": "Vegeta HTTP load test",
		"rates":              rates,
		"duration_seconds":   duration.Seconds(),
		"target_endpoint":    "POST /identify",
		"results":            results,
	}

	writeJSON(filepath.Join(runDir, "BM8_LOADTEST.json"), res)
}