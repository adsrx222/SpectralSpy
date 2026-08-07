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

	"github.com/adsrx222/SpectralSpy/db"
	"github.com/adsrx222/SpectralSpy/SpectralSpy"
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

type SongMetadataInfo struct {
	SongID        string `json:"song_id"`
	Artist        string `json:"artist"`
	Title         string `json:"title"`
	Year          int    `json:"year"`
	MidiFilename  string `json:"midi_filename"`
	AudioFilename string `json:"audio_filename"`
	IsNoise       bool   `json:"is_noise"`
	NoiseType     string `json:"noise_type"`
}

type EvaluationQuery struct {
	Samples        []float64
	ExpectedSongID string
	StartOffsetSec float64
	DurationSec    float64
	DistortionType string
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

func AddNoise(signal []float64, snrDB float64, rng *rand.Rand) []float64 {
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

func SimulateCompression(signal []float64, bitrateKbps float64) []float64 {
	bitDepth := 8.0
	cutoffHz := 16000.0
	
	if bitrateKbps <= 64.0 {
		bitDepth = 6.0
		cutoffHz = 10000.0
	}

	steps := math.Pow(2, bitDepth)
	compressed := make([]float64, len(signal))
	
	dt := 1.0 / float64(SpectralSpy.SAMPLE_RATE)
	rc := 1.0 / (2 * math.Pi * cutoffHz)
	alpha := dt / (rc + dt)

	prev := 0.0
	for i, s := range signal {
		quantized := math.Round(s*steps) / steps
		val := prev + alpha*(quantized-prev)
		compressed[i] = val
		prev = val
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

func GetContinuousSwath(fingerprints []SpectralSpy.Fingerprint, maxLen int) []SpectralSpy.Fingerprint {
	if len(fingerprints) <= maxLen {
		return fingerprints
	}
	maxStart := len(fingerprints) - maxLen
	startIdx := rand.Intn(maxStart + 1)
	return fingerprints[startIdx : startIdx+maxLen]
}

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

func SetupMockS3(t *testing.T) *httptest.Server {
	if t != nil {
		t.Helper()
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mock data"))
	}))
}

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

	// check if the WAV file is already extracted on disk
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

	// extract single target file to disk cache
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

func GetStratumKey(composer, title string, year int) string {
	genre := composer
	
	instrumentation := "modern"
	if year < 2008 {
		instrumentation = "early"
	} else if year < 2014 {
		instrumentation = "mid"
	}

	titleLower := strings.ToLower(title)
	tempo := "medium"
	if strings.Contains(titleLower, "allegro") || strings.Contains(titleLower, "presto") || strings.Contains(titleLower, "vivace") || strings.Contains(titleLower, "scherzo") {
		tempo = "fast"
	} else if strings.Contains(titleLower, "adagio") || strings.Contains(titleLower, "andante") || strings.Contains(titleLower, "lento") || strings.Contains(titleLower, "grave") {
		tempo = "slow"
	}
	
	return fmt.Sprintf("%s|%s|%s", genre, instrumentation, tempo)
}

func GenerateSilence(durationSec float64, sampleRate int) []float64 {
	length := int(durationSec * float64(sampleRate))
	return make([]float64, length)
}

func GenerateRepetitiveKickDrum(durationSec float64, sampleRate int) []float64 {
	length := int(durationSec * float64(sampleRate))
	samples := make([]float64, length)
	
	bpm := 120.0
	intervalSamples := int(float64(sampleRate) * 60.0 / bpm) // 0.5 seconds of samples
	kickDurSamples := int(float64(sampleRate) * 0.15)        // 150 ms kick duration
	
	for i := 0; i < length; i += intervalSamples {
		for j := 0; j < kickDurSamples && i+j < length; j++ {
			t := float64(j) / float64(sampleRate)
			// frequency sweeps down from 150Hz to 45Hz
			freq := 45.0 + (150.0-45.0)*math.Exp(-t*40.0)
			// apply exponential amplitude envelope to decay the kick drum
			env := math.Exp(-t * 20.0)
			samples[i+j] = math.Sin(2.0*math.Pi*freq*t) * env
		}
	}
	return samples
}

func StratifiedSampleMaestro(workspaceDir string, samplePercentage float64) ([]SongMetadataInfo, error) {
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	file, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("reading json: %w", err)
	}

	var df MaestroDataframe
	if err := json.Unmarshal(file, &df); err != nil {
		return nil, fmt.Errorf("unmarshaling json dataframe: %w", err)
	}

	// group songs by stratum key
	groups := make(map[string][]SongMetadataInfo)
	for key, audioPath := range df.AudioFilename {
		composer := df.CanonicalComposer[key]
		title := df.CanonicalTitle[key]
		year := df.Year[key]
		midiPath := df.MidiFilename[key]
		
		hash64 := xxh3.HashString(audioPath)
		songID := strconv.FormatUint(hash64, 36)

		song := SongMetadataInfo{
			SongID:        songID,
			Artist:        composer,
			Title:         title,
			Year:          year,
			MidiFilename:  midiPath,
			AudioFilename: audioPath,
			IsNoise:       false,
		}

		stratum := GetStratumKey(composer, title, year)
		groups[stratum] = append(groups[stratum], song)
	}

	var sampled []SongMetadataInfo
	rng := rand.New(rand.NewSource(42))

	for _, songs := range groups {
		groupSize := len(songs)
		sampleCount := int(math.Round(float64(groupSize) * samplePercentage))
		if sampleCount < 1 && groupSize > 0 {
			sampleCount = 1
		}
		
		indices := rng.Perm(groupSize)
		for idx := 0; idx < sampleCount && idx < groupSize; idx++ {
			sampled = append(sampled, songs[indices[idx]])
		}
	}

	noiseTracks := []SongMetadataInfo{
		{
			SongID:    "noise_silence",
			Artist:    "Synthetic",
			Title:     "Classical Silence",
			IsNoise:   true,
			NoiseType: "silence",
		},
		{
			SongID:    "noise_kickdrum",
			Artist:    "Synthetic",
			Title:     "Electronic Kick Drums",
			IsNoise:   true,
			NoiseType: "kick_drum",
		},
	}
	sampled = append(sampled, noiseTracks...)

	return sampled, nil
}

func GetAudioForTrack(workspaceDir string, track SongMetadataInfo) ([]float64, error) {
	if track.IsNoise {
		if track.NoiseType == "silence" {
			return GenerateSilence(30.0, SpectralSpy.SAMPLE_RATE), nil
		}
		if track.NoiseType == "kick_drum" {
			return GenerateRepetitiveKickDrum(30.0, SpectralSpy.SAMPLE_RATE), nil
		}
		return nil, fmt.Errorf("unknown noise type: %s", track.NoiseType)
	}

	extractedPath := filepath.Join(workspaceDir, track.AudioFilename)
	if wavFile, err := os.Open(extractedPath); err == nil {
		defer wavFile.Close()
		return DecodeWavToFloat64(wavFile)
	}

	zipPath := filepath.Join(workspaceDir, "maestro-v3.0.0.zip")
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("opening audio zip: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, track.AudioFilename) {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("opening wav %s from zip: %w", f.Name, err)
			}
			defer rc.Close()

			if err := os.MkdirAll(filepath.Dir(extractedPath), 0755); err != nil {
				return nil, fmt.Errorf("creating directories: %w", err)
			}

			outFn, err := os.Create(extractedPath)
			if err != nil {
				return nil, fmt.Errorf("creating wav file: %w", err)
			}
			if _, err := io.Copy(outFn, rc); err != nil {
				outFn.Close()
				return nil, fmt.Errorf("writing wav file: %w", err)
			}
			outFn.Close()

			wavFile, err := os.Open(extractedPath)
			if err != nil {
				return nil, fmt.Errorf("opening extracted wav file: %w", err)
			}
			defer wavFile.Close()

			return DecodeWavToFloat64(wavFile)
		}
	}

	return nil, fmt.Errorf("track not found: %s", track.AudioFilename)
}

func ApplyEQCut(signal []float64, cutoffHz float64, filterType string) []float64 {
	out := make([]float64, len(signal))
	dt := 1.0 / float64(SpectralSpy.SAMPLE_RATE)
	rc := 1.0 / (2 * math.Pi * cutoffHz)
	
	alphaLP := dt / (rc + dt)
	alphaHP := rc / (rc + dt)

	if filterType == "lowpass" {
		prev := 0.0
		for i, x := range signal {
			val := prev + alphaLP*(x-prev)
			out[i] = val
			prev = val
		}
	} else if filterType == "highpass" {
		prevX := 0.0
		prevY := 0.0
		for i, x := range signal {
			val := alphaHP * (prevY + x - prevX)
			out[i] = val
			prevX = x
			prevY = val
		}
	} else {
		copy(out, signal)
	}
	return out
}

func ApplyRoomReverb(signal []float64, delayMs []float64, decay []float64) []float64 {
	out := make([]float64, len(signal))
	copy(out, signal)

	for idx, delayTime := range delayMs {
		delaySamples := int(delayTime * float64(SpectralSpy.SAMPLE_RATE) / 1000.0)
		g := decay[idx]
		for i := delaySamples; i < len(signal); i++ {
			out[i] += g * out[i-delaySamples]
		}
	}
	return out
}

func GenerateEvaluationQuerySet(workspaceDir string, corpus []SongMetadataInfo, queryCount int) ([]EvaluationQuery, error) {
	rng := rand.New(rand.NewSource(1337)) // Deterministic seed
	queries := make([]EvaluationQuery, 0, queryCount)

	var validTracks []SongMetadataInfo
	for _, track := range corpus {
		validTracks = append(validTracks, track)
	}

	if len(validTracks) == 0 {
		return nil, fmt.Errorf("no tracks in corpus to select from")
	}

	distortions := []string{"clean", "whitenoise", "eq_lowpass", "eq_highpass", "reverb"}

	for len(queries) < queryCount {
		track := validTracks[rng.Intn(len(validTracks))]
		samples, err := GetAudioForTrack(workspaceDir, track)
		if err != nil {
			continue
		}

		durationSamples := len(samples)
		if durationSamples < 10*SpectralSpy.SAMPLE_RATE {
			continue
		}

		durSec := 3.0 + rng.Float64()*7.0
		durSamples := int(durSec * float64(SpectralSpy.SAMPLE_RATE))

		maxStart := durationSamples - durSamples
		startSample := rng.Intn(maxStart)
		startSec := float64(startSample) / float64(SpectralSpy.SAMPLE_RATE)

		snippet := make([]float64, durSamples)
		copy(snippet, samples[startSample:startSample+durSamples])

		distType := distortions[rng.Intn(len(distortions))]
		var distorted []float64

		switch distType {
		case "clean":
			distorted = snippet
		case "whitenoise":
			snr := 10.0 + rng.Float64()*20.0
			distorted = AddNoise(snippet, snr, rng)
		case "eq_lowpass":
			distorted = ApplyEQCut(snippet, 1500.0, "lowpass")
		case "eq_highpass":
			distorted = ApplyEQCut(snippet, 300.0, "highpass")
		case "reverb":
			distorted = ApplyRoomReverb(snippet, []float64{30, 60, 90}, []float64{0.4, 0.2, 0.1})
		default:
			distorted = snippet
		}

		queries = append(queries, EvaluationQuery{
			Samples:        distorted,
			ExpectedSongID: track.SongID,
			StartOffsetSec: startSec,
			DurationSec:    durSec,
			DistortionType: distType,
		})
	}

	return queries, nil
}