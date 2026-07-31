package testutil

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	_ "github.com/mattn/go-sqlite3"
	"github.com/zeebo/xxh3"

	"spectralspy/db"
	"spectralspy/pkg/SpectralSpy"
	"spectralspy/server"
)

// MaestroDataframe maps the JSON structure of the dataset.
type MaestroDataframe struct {
	CanonicalComposer map[string]string `json:"canonical_composer"`
	CanonicalTitle    map[string]string `json:"canonical_title"`
	Year              map[string]int    `json:"year"`
	MidiFilename      map[string]string `json:"midi_filename"`
	AudioFilename     map[string]string `json:"audio_filename"`
}

type Stats struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	StdDev float64 `json:"stddev"`
}

type MetricReport struct {
	Name     string
	Category string
	Stats    Stats
}

type AccuracyResult struct {
	Category  string
	Top1      float64
	Precision float64
	Recall    float64
	F1Score   float64
	FalsePos  float64
	FalseNeg  float64
}

func CalculateStats(data []float64) Stats {
	if len(data) == 0 {
		return Stats{}
	}
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))

	var varianceSum float64
	for _, v := range sorted {
		varianceSum += (v - mean) * (v - mean)
	}

	return Stats{
		Count:  len(sorted),
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
		Mean:   mean,
		Median: Percentile(sorted, 50),
		P50:    Percentile(sorted, 50),
		P90:    Percentile(sorted, 90),
		P95:    Percentile(sorted, 95),
		P99:    Percentile(sorted, 99),
		StdDev: math.Sqrt(varianceSum / float64(len(sorted))),
	}
}

func Percentile(sorted []float64, p float64) float64 {
	index := (p / 100.0) * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	if lower == upper {
		return sorted[lower]
	}
	weight := index - float64(lower)
	return sorted[lower]*(1.0-weight) + sorted[upper]*weight
}

func AddNoise(signal []float64, snrDB float64) []float64 {
	var signalPower float64
	for _, s := range signal {
		signalPower += s * s
	}
	signalPower /= float64(len(signal))

	noisePower := signalPower / math.Pow(10, snrDB/10)
	noiseAmp := math.Sqrt(noisePower)

	noisy := make([]float64, len(signal))
	for i, s := range signal {
		noisy[i] = s + (rand.NormFloat64() * noiseAmp)
	}
	return noisy
}

func SimulateCompression(signal []float64, cutoffHz float64) []float64 {
	windowSize := int(SpectralSpy.SAMPLE_RATE / cutoffHz)
	if windowSize < 1 {
		windowSize = 1
	}
	compressed := make([]float64, len(signal))
	var sum float64
	for i := 0; i < len(signal); i++ {
		sum += signal[i]
		if i >= windowSize {
			sum -= signal[i-windowSize]
		}
		compressed[i] = sum / float64(windowSize)
	}
	return compressed
}

func ResampleAudio(signal []float64, rateMultiplier float64) []float64 {
	newLength := int(float64(len(signal)) / rateMultiplier)
	resampled := make([]float64, newLength)
	for i := range resampled {
		origIdx := float64(i) * rateMultiplier
		lower := int(math.Floor(origIdx))
		upper := int(math.Ceil(origIdx))
		if upper >= len(signal) {
			upper = len(signal) - 1
		}
		weight := origIdx - float64(lower)
		resampled[i] = signal[lower]*(1.0-weight) + signal[upper]*weight
	}
	return resampled
}

func GetContinuousSwath(fingerprints []server.Fingerprint, maxLen int) []server.Fingerprint {
	if len(fingerprints) <= maxLen {
		return fingerprints
	}
	maxStart := len(fingerprints) - maxLen
	startIdx := rand.Intn(maxStart + 1)
	return fingerprints[startIdx : startIdx+maxLen]
}

// SetupTestDB returns a raw *sql.DB.
func SetupTestDB(t *testing.T, dbPath string) *sql.DB {
	if t != nil {
		t.Helper()
	}
	
	dsn := fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL&_busy_timeout=5000", dbPath)
	dbConn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		panic(fmt.Errorf("failed to open db: %w", err))
	}
	
	if dbPath == "" {
		if err := db.InitSchema(dbConn); err != nil {
			if t != nil {
				t.Fatalf("Failed to init schema: %v", err)
			} else {
				panic(fmt.Errorf("failed to init schema: %w", err))
			}
		}
	}
	return dbConn
}

// SetupMockS3 returns standard HTTP server types for S3 mocking.
func SetupMockS3(t *testing.T) *httptest.Server {
	if t != nil {
		t.Helper()
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mock data"))
	}))
}

// DecodeWavToFloat64 converts raw PCM audio data into float64 slices.
func DecodeWavToFloat64(r io.Reader) ([]float64, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("failed to buffer stream for seeker: %w", err)
		}
		rs = bytes.NewReader(b)
	}

	d := wav.NewDecoder(rs)
	if !d.IsValidFile() {
		return nil, fmt.Errorf("invalid WAV stream")
	}

	format := d.Format()
	if format == nil {
		return nil, fmt.Errorf("could not parse WAV format")
	}

	numChannels := format.NumChannels
	bitDepth := d.BitDepth

	var maxVal float64
	switch bitDepth {
	case 8:
		maxVal = 128.0
	case 16:
		maxVal = 32768.0
	case 24:
		maxVal = 8388608.0
	case 32:
		maxVal = 2147483648.0
	default:
		return nil, fmt.Errorf("unsupported bit depth: %d", bitDepth)
	}

	var samples []float64
	buf := &audio.IntBuffer{
		Format:         format,
		Data:           make([]int, 8192*numChannels),
		SourceBitDepth: int(bitDepth),
	}

	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			return nil, fmt.Errorf("failed to read PCM buffer: %w", err)
		}
		if n == 0 {
			break
		}
		for i := 0; i < n; i += numChannels {
			var sum float64
			for c := 0; c < numChannels; c++ {
				sum += float64(buf.Data[i+c]) / maxVal
			}
			samples = append(samples, sum/float64(numChannels))
		}
	}
	return samples, nil
}

// GetRandomMaestroSong reads the JSON dataset, selects a song, extracts its WAV directly
// to disk if not already cached, and returns the raw float64 samples alongside its computed ID.
func GetRandomMaestroSong(workspaceDir string) ([]float64, string, error) {
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	file, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, "", fmt.Errorf("reading json: %w", err)
	}

	var df MaestroDataframe
	if err := json.Unmarshal(file, &df); err != nil {
		return nil, "", fmt.Errorf("unmarshaling json dataframe: %w", err)
	}

	var keys []string
	for k := range df.AudioFilename {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, "", fmt.Errorf("no songs found in maestro JSON")
	}

	rand.Seed(time.Now().UnixNano())
	randomKey := keys[rand.Intn(len(keys))]
	wavPath := df.AudioFilename[randomKey]

	hash64 := xxh3.HashString(wavPath)
	songID := strconv.FormatUint(hash64, 36)

	// Check if the WAV file is already extracted on disk
	extractedPath := filepath.Join(workspaceDir, wavPath)
	if wavFile, err := os.Open(extractedPath); err == nil {
		fmt.Printf("  [testutil] Reading pre-extracted WAV directly from disk: %s\n", extractedPath)
		defer wavFile.Close()
		samples, err := DecodeWavToFloat64(wavFile)
		if err != nil {
			return nil, "", fmt.Errorf("decoding cached wav: %w", err)
		}
		return samples, songID, nil
	}

	// Extract single target file to disk cache
	fmt.Printf("  [testutil] Unzipping target track to disk cache: %s\n", wavPath)
	zipPath := filepath.Join(workspaceDir, "maestro-v3.0.0.zip")
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, "", fmt.Errorf("opening audio zip: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, wavPath) {
			rc, err := f.Open()
			if err != nil {
				return nil, "", fmt.Errorf("opening wav %s from zip: %w", f.Name, err)
			}
			defer rc.Close()

			if err := os.MkdirAll(filepath.Dir(extractedPath), 0755); err != nil {
				return nil, "", fmt.Errorf("creating dirs for extracted wav: %w", err)
			}

			outFn, err := os.Create(extractedPath)
			if err != nil {
				return nil, "", fmt.Errorf("creating extracted wav file: %w", err)
			}

			if _, err := io.Copy(outFn, rc); err != nil {
				outFn.Close()
				return nil, "", fmt.Errorf("writing extracted wav file: %w", err)
			}
			outFn.Close()

			wavFile, err := os.Open(extractedPath)
			if err != nil {
				return nil, "", fmt.Errorf("opening newly extracted wav file: %w", err)
			}
			defer wavFile.Close()

			samples, err := DecodeWavToFloat64(wavFile)
			if err != nil {
				return nil, "", fmt.Errorf("decoding wav: %w", err)
			}
			return samples, songID, nil
		}
	}

	return nil, "", fmt.Errorf("wav file %s not found in archive", wavPath)
}