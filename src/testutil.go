package SpectralSpy

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	_ "github.com/mattn/go-sqlite3"
)

const WAVEFORM_PATH = "../live-demo/workspace/waveforms"

// MATH UTILS

func percentile(sorted []float64, p float64) float64 {
	index := (p / 100.0) * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	if lower == upper {
		return sorted[lower]
	}
	weight := index - float64(lower)
	return sorted[lower]*(1.0-weight) + sorted[upper]*weight
}

func avg(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func getMin(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	min := data[0]
	for _, v := range data {
		if v < min {
			min = v
		}
	}
	return min
}

func getMax(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	max := data[0]
	for _, v := range data {
		if v > max {
			max = v
		}
	}
	return max
}

// RANDOM SAMPLING UTILS

func findRandomClip(waveformPath string) (string, error) {
	entries, err := os.ReadDir(waveformPath)
	if err != nil {
		return "", fmt.Errorf("reading directory: %w", err)
	}

	var wavFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if strings.EqualFold(filepath.Ext(entry.Name()), ".wav") {
			wavFiles = append(wavFiles, entry.Name())
		}
	}

	if len(wavFiles) == 0 {
		return "", fmt.Errorf("no .wav files found in directory: %s", waveformPath)
	}

	idx := rand.Intn(len(wavFiles))

	return filepath.Join(waveformPath, wavFiles[idx]), nil
}

func findRandomPath(waveformPath string, clipLen int) (string, error) {
	// Find a random WAV file.
	randomWav, err := findRandomClip(waveformPath)
	if err != nil {
		return "", fmt.Errorf("error finding random waveform: %w", err)
	}

	waveform, err := processWAV(randomWav)
	if err != nil {
		return "", fmt.Errorf("error processing waveform: %w", err)
	}

	const sampleRate = 44100
	clipSamples := clipLen * sampleRate

	if clipSamples <= 0 {
		return "", fmt.Errorf("invalid clip length: %d seconds", clipLen)
	}

	if len(waveform) < clipSamples {
		return "", fmt.Errorf(
			"waveform is too short: %.2f seconds, requested %d seconds",
			float64(len(waveform))/sampleRate,
			clipLen,
		)
	}

	// pick a random starting point.
	maxStart := len(waveform) - clipSamples
	start := rand.Intn(maxStart + 1)
	end := start + clipSamples

	clip := waveform[start:end]

	// create temporary WAV file.
	tmpFile, err := os.CreateTemp("", "clean-*.wav")
	if err != nil {
		return "", fmt.Errorf("creating temporary WAV: %w", err)
	}

	tmpPath := tmpFile.Name()

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("closing temporary WAV: %w", err)
	}

	// write the clip to the temporary WAV
	if err := writeWAV(tmpPath, clip, sampleRate); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("writing temporary WAV: %w", err)
	}

	return tmpPath, nil
}

func findRandomSlice(waveformPath string, clipLen int) ([]float64, error) {
	randomSong, err := findRandomClip(waveformPath)
	if err != nil {
		return nil, fmt.Errorf("error finding waveform: %w", err)
	}

	waveform, err := processWAV(randomSong)
	if err != nil {
		return nil, fmt.Errorf(
			"error processing waveform %s: %w",
			randomSong,
			err,
		)
	}

	const sampleRate = 44100
	clipSamples := clipLen * sampleRate

	if clipSamples <= 0 {
		return nil, fmt.Errorf(
			"invalid clip length: %d seconds",
			clipLen,
		)
	}

	if len(waveform) < clipSamples {
		return nil, fmt.Errorf(
			"waveform is too short: %d samples (%.2f seconds), requested %d seconds",
			len(waveform),
			float64(len(waveform))/sampleRate,
			clipLen,
		)
	}

	maxStart := len(waveform) - clipSamples
	start := rand.Intn(maxStart + 1)
	end := start + clipSamples

	clip := make([]float64, clipSamples)
	copy(clip, waveform[start:end])

	return clip, nil
}

// AUDIO PROCESSING UTILS

func makeSilence(n int) []float64 {
	return make([]float64, n)
}

func makeSine(n int, freq float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Sin(2 * math.Pi * freq * float64(i) / SAMPLE_RATE)
	}
	return out
}

func addNoise(signal []float64, snrDB float64, rng *rand.Rand) []float64 {
	var signalPower float64
	for _, s := range signal {
		signalPower += s * s
	}
	signalPower /= float64(len(signal))

	noisePower := signalPower / math.Pow(10, snrDB/10)
	noiseAmp := math.Sqrt(noisePower)

	noisy := make([]float64, len(signal))
	for i, s := range signal {
		var norm float64
		if rng != nil {
			norm = rng.NormFloat64()
		} else {
			norm = rand.NormFloat64()
		}
		noisy[i] = s + (norm * noiseAmp)
	}
	return noisy
}

func resampleAudio(input []float64, inputRate int, outputRate int) ([]float64, error) {
	if inputRate == outputRate {
		return input, nil
	}

	ratio := float64(outputRate) / float64(inputRate)
	outputLen := int(math.Ceil(float64(len(input)) * ratio))

	output := make([]float64, outputLen)

	// linear interpolation
	for i := range output {
		pos := float64(i) / ratio

		i0 := int(pos)
		i1 := i0 + 1

		if i0 >= len(input) {
			i0 = len(input) - 1
		}
		if i1 >= len(input) {
			i1 = len(input) - 1
		}

		t := pos - float64(i0)

		output[i] = input[i0]*(1-t) + input[i1]*t
	}

	return output, nil
}

func normalizePeak(samples []float64) {
	var peak float64
	for _, x := range samples {
		if math.Abs(x) > peak {
			peak = math.Abs(x)
		}
	}

	if peak == 0 {
		return
	}

	gain := 0.99 / peak

	for i := range samples {
		samples[i] *= gain
	}
}

// AUDIO FILE OPERATIONS

func runFFmpegTranscode(inputFile string, format string, bitrate string) ([]float64, error) {
	tempFormat := filepath.Join(os.TempDir(), fmt.Sprintf("bench_%d.%s", time.Now().UnixNano(), format))
	defer os.Remove(tempFormat)

	cmd := exec.Command("ffmpeg", "-y", "-i", inputFile, "-b:a", bitrate, tempFormat)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg error: %v\nOutput: %s", err, string(out))
	}

	tempWav := filepath.Join(os.TempDir(), fmt.Sprintf("bench_decode_%d.wav", time.Now().UnixNano()))
	defer os.Remove(tempWav)

	cmd2 := exec.Command("ffmpeg", "-y", "-i", tempFormat, tempWav)
	if out, err := cmd2.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg decode error: %v\nOutput: %s", err, string(out))
	}
	return processWAV(tempWav)
}

func runFFmpegFilter(inputFile string, filter string) ([]float64, error) {
	outputFile := filepath.Join(os.TempDir(), fmt.Sprintf("bench_%d.wav", time.Now().UnixNano()))
	defer os.Remove(outputFile)
	cmd := exec.Command("ffmpeg", "-y", "-i", inputFile, "-af", filter, outputFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg filter error: %v\nOutput: %s", err, string(out))
	}
	return processWAV(outputFile)
}

func processWAV(path string) ([]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	decoder := wav.NewDecoder(f)
	if !decoder.IsValidFile() {
		return nil, fmt.Errorf("Invalid WAV File: %s", path)
	}

	buf, err := decoder.FullPCMBuffer()
	if err != nil {
		return nil, err
	}

	if buf == nil || len(buf.Data) == 0 {
		return nil, fmt.Errorf("Empty WAV File: %s", path)
	}

	channels := buf.Format.NumChannels
	sampleRate := buf.Format.SampleRate

	// convert PCM integer samples to float64
	scale := float64(int64(1) << (buf.SourceBitDepth - 1))

	// convert to mono
	mono := make([]float64, 0, len(buf.Data)/channels)

	for i := 0; i < len(buf.Data); i += channels {
		var sum float64
		for c := 0; c < channels; c++ {
			sum += float64(buf.Data[i+c]) / scale
		}
		mono = append(mono, sum/float64(channels))
	}

	// resample to 44100 hz
	resampled, err := resampleAudio(mono, sampleRate, 44100)
	if err != nil {
		return nil, err
	}

	// peak-normalize
	normalizePeak(resampled)

	return resampled, nil
}

func writeWAV(path string, samples []float64, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	encoder := wav.NewEncoder(
		f,
		sampleRate,
		16,
		1,
		1,
	)

	// convert float64 samples [-1, 1] to int16 PCM
	data := make([]int, len(samples))

	for i, sample := range samples {
		if sample > 1.0 {
			sample = 1.0
		} else if sample < -1.0 {
			sample = -1.0
		}

		data[i] = int(sample * 32767.0)
	}

	buffer := &audio.IntBuffer{
		Data: data,
		Format: &audio.Format{
			NumChannels: 1,
			SampleRate:  sampleRate,
		},
		SourceBitDepth: 16,
	}

	if err := encoder.Write(buffer); err != nil {
		return err
	}

	return encoder.Close()
}

// DATABASE UTILS

func setupTestDB(t *testing.T) *sql.DB {
	if t != nil {
		t.Helper()
	}

	var dsn string
	if t != nil {
		// Create a temporary directory that is automatically cleaned up after the test
		dbPath := filepath.Join(t.TempDir(), "test.db")
		dsn = fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL&_busy_timeout=5000", dbPath)
	} else {
		// Fallback to an in-memory database if no test context is provided
		dsn = "file::memory:?cache=shared&_journal_mode=WAL&_busy_timeout=5000"
	}

	dbConn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		if t != nil {
			t.Fatalf("failed to open db: %v", err)
		}
		panic(fmt.Errorf("failed to open db: %w", err))
	}

	// Since it is always temporary, we always initialize the schema
	if err := InitSchema(dbConn); err != nil {
		if t != nil {
			t.Fatalf("Failed to init schema: %v", err)
		} else {
			panic(fmt.Errorf("failed to init schema: %w", err))
		}
	}

	return dbConn
}

func getDBConn(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL&_busy_timeout=5000", dbPath)
	dbConn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	return dbConn, nil
}

// MISC

func writeJSON(path string, data interface{}) {
	b, _ := json.MarshalIndent(data, "", "  ")
	os.WriteFile(path, b, 0644)
}

func hashesFrom(entries []Fingerprint) []uint64 {
	out := make([]uint64, len(entries))
	for i, e := range entries {
		out[i] = e.Hash
	}
	return out
}

func hashSet(entries []Fingerprint) map[uint64]struct{} {
	m := make(map[uint64]struct{}, len(entries))
	for _, e := range entries {
		m[e.Hash] = struct{}{}
	}
	return m
}

func makeSpectrogram(samples []float64) Spectrogram {
	ctx := context.Background()
	return buildSpectrogram(ctx, samples)
}

func getSongID(wavPath string) string {
	hashArray := sha1.Sum([]byte(wavPath))
	return hex.EncodeToString(hashArray[:])
}