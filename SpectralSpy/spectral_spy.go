package SpectralSpy

import (
	"context"
	"encoding/binary"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/zeebo/xxh3"
	"gonum.org/v1/gonum/dsp/fourier"
)

const SAMPLE_RATE = 44100
const WINDOW_SIZE = 4096
const HOP_SIZE = WINDOW_SIZE / 2

const PEAK_NEIGHBORHOOD_TIME = 5 // ±frames
const PEAK_NEIGHBORHOOD_FREQ = 5 // ±FFT bins

const TARGET_ZONE_TIME_START = 2 // frames ahead where zone begins
const TARGET_ZONE_TIME_END = 20   // frames ahead where zone ends
const TARGET_ZONE_FREQ_BINS = 5 // ±FFT bins around anchor frequency bin
const MAX_FAN_OUT = 10

const MAX_PEAKS_PER_FRAME = 5 // Top-K frame-level rate limit
const CFAR_TRAIN_BINS = 8      // CFAR noise estimation window size
const CFAR_GUARD_BINS = 2      // CFAR guard window size
const CFAR_FACTOR = 1.25       // SNR factor over dynamic noise floor

var HANN_WINDOW = func() []float64 {
	w := make([]float64, WINDOW_SIZE)
	for n := range WINDOW_SIZE {
		w[n] = 0.5 * (1 - math.Cos(2*math.Pi*float64(n)/float64(WINDOW_SIZE-1)))
	}
	return w
}()

// low pass filter
var FREQ_WEIGHTS = func() []float64 {
	numBins := WINDOW_SIZE / 2
	weights := make([]float64, numBins)
	binHz := float64(SAMPLE_RATE) / float64(WINDOW_SIZE)

	for k := 0; k < numBins; k++ {
		freq := float64(k) * binHz
		if freq < 150.0 {
			weights[k] = math.Pow(freq/150.0, 2.0)
		} else if freq <= 5000.0 {
			weights[k] = 1.0
		} else if freq <= 8000.0 {
			weights[k] = math.Exp(-(freq - 5000.0) / 1000.0)
		} else {
			weights[k] = 0.0 // cutoff above 8kHz
		}
	}
	return weights
}()

type Spectrogram struct {
	Mags  [][]float64
	Times []float64
	BinHz float64
}

type ConstellationPoint struct {
	Timestamp float64
	Frequency float64
	Magnitude float64
	BinIndex  int
}

type Fingerprint struct {
	Hash       uint64  `json:"hash,string"`
	AnchorTime float64 `json:"anchor_time"`
}

type DBEntry struct {
	Hash       uint64
	SongID     string
	AnchorTime float64
	Weight     float64
}

type OffsetKey struct {
	SongID string
	Bin    uint64
}

type StageMetrics struct {
	SpectrogramDuration time.Duration `json:"spectrogram_duration"`
	PeakFindDuration    time.Duration `json:"peak_find_duration"`
	HashGenDuration     time.Duration `json:"hash_gen_duration"`
	TotalDuration       time.Duration `json:"total_duration"`
}

func buildSpectrogram(ctx context.Context, samples []float64) Spectrogram {
	numSamples := len(samples)
	if numSamples == 0 {
		return Spectrogram{BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE)}
	}

	numFrames := (numSamples + HOP_SIZE - 1) / HOP_SIZE
	mags := make([][]float64, numFrames)
	times := make([]float64, numFrames)

	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		fft := fourier.NewFFT(WINDOW_SIZE)

		startFrame := w * framesPerWorker
		endFrame := min((startFrame + framesPerWorker), numFrames)

		if startFrame >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			chunk := make([]float64, WINDOW_SIZE)

			for frame := start; frame < end; frame++ {
				if ctx.Err() != nil {
					return
				}

				i := frame * HOP_SIZE

				for j := 0; j < WINDOW_SIZE; j++ {
					if i+j < numSamples {
						chunk[j] = samples[i+j] * HANN_WINDOW[j]
					} else {
						chunk[j] = 0.0
					}
				}

				coeffs := fft.Coefficients(nil, chunk)
				magSlice := make([]float64, len(coeffs)/2)

				for k := range magSlice {
					re := real(coeffs[k])
					im := imag(coeffs[k])

					magSlice[k] = (re*re + im*im) * FREQ_WEIGHTS[k] // pre-filtering
				}

				mags[frame] = magSlice
				times[frame] = float64(i) + float64(WINDOW_SIZE)/2
			}
		}(startFrame, endFrame)
	}

	wg.Wait()

	return Spectrogram{
		Mags:  mags,
		Times: times,
		BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE),
	}
}

func findPeaks(ctx context.Context, sg Spectrogram) [][]ConstellationPoint {
	numFrames := len(sg.Mags)
	if numFrames == 0 {
		return nil
	}

	numBins := len(sg.Mags[0])
	peaks := make([][]ConstellationPoint, numFrames)
	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		startFrame := w * framesPerWorker
		endFrame := min((startFrame + framesPerWorker), numFrames)

		if startFrame >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()

			for t := start; t < end; t++ {
				if ctx.Err() != nil {
					return
				}

				var localPeaks []ConstellationPoint

				tMin := max(0, t-PEAK_NEIGHBORHOOD_TIME)
				tMax := min(numFrames-1, t+PEAK_NEIGHBORHOOD_TIME)

				for f := 0; f < numBins; f++ {
					mag := sg.Mags[t][f]
					if mag <= 0 {
						continue
					}

					// calculate local neighborhood average
					fMin := max(0, f-PEAK_NEIGHBORHOOD_FREQ)
					fMax := min(numBins-1, f+PEAK_NEIGHBORHOOD_FREQ)

					var localSum float64
					var count int

					isMax := true
					for nt := tMin; nt <= tMax && isMax; nt++ {
						for nf := fMin; nf <= fMax; nf++ {
							neighborMag := sg.Mags[nt][nf]
							localSum += neighborMag
							count++

							// strict local maxima check
							if neighborMag > mag {
								isMax = false
							}
						}
					}

					// only keep peak if it's the strict maximum AND exceeds the local average * CFAR_FACTOR
					localAvg := localSum / float64(count)
					if isMax && mag > (localAvg*CFAR_FACTOR) {
						localPeaks = append(localPeaks, ConstellationPoint{
							Timestamp: sg.Times[t],
							Frequency: float64(f) * sg.BinHz,
							Magnitude: mag,
							BinIndex:  f,
						})
					}
				}

				if len(localPeaks) > 0 {
					// Optional: Sort by magnitude and enforce MAX_PEAKS_PER_FRAME[cite: 18]
					if len(localPeaks) > MAX_PEAKS_PER_FRAME {
						sort.Slice(localPeaks, func(i, j int) bool {
							return localPeaks[i].Magnitude > localPeaks[j].Magnitude
						})
						localPeaks = localPeaks[:MAX_PEAKS_PER_FRAME]
					}
					peaks[t] = localPeaks
				}
			}
		}(startFrame, endFrame)
	}

	wg.Wait()
	return peaks
}

func hashPair(anchor, tether ConstellationPoint) uint64 {
	var buf [24]byte

	binary.LittleEndian.PutUint64(buf[0:8], math.Float64bits(anchor.Frequency))
	binary.LittleEndian.PutUint64(buf[8:16], math.Float64bits(tether.Frequency))
	binary.LittleEndian.PutUint64(buf[16:24], math.Float64bits(tether.Timestamp-anchor.Timestamp))

	return xxh3.Hash(buf[:])
}

func generateHashEntries(ctx context.Context, peaks [][]ConstellationPoint) []Fingerprint {
	numFrames := len(peaks)
	if numFrames == 0 {
		return nil
	}

	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	results := make([][]Fingerprint, numWorkers)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		start := w * framesPerWorker
		end := min(start+framesPerWorker, numFrames)

		if start >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end, workerID int) {
			defer wg.Done()

			local := make([]Fingerprint, 0, 1024)

			for anchorT := start; anchorT < end; anchorT++ {
				if ctx.Err() != nil {
					return
				}

				tetherStart := anchorT + TARGET_ZONE_TIME_START
				tetherEnd := min(anchorT+TARGET_ZONE_TIME_END, numFrames-1)

				for _, anchor := range peaks[anchorT] {
					minBin := anchor.BinIndex - TARGET_ZONE_FREQ_BINS
					maxBin := anchor.BinIndex + TARGET_ZONE_FREQ_BINS

					matches := 0

				tetherSearch:
					for tetherT := tetherStart; tetherT <= tetherEnd; tetherT++ {
						for _, tether := range peaks[tetherT] {
							if tether.BinIndex < minBin {
								continue
							}
							if tether.BinIndex > maxBin {
								break
							}

							local = append(local, Fingerprint{
								Hash:       hashPair(anchor, tether),
								AnchorTime: anchor.Timestamp,
							})

							matches++
							if matches >= MAX_FAN_OUT {
								break tetherSearch
							}
						}
					}
				}
			}

			results[workerID] = local
		}(start, end, w)
	}

	wg.Wait()

	totalLen := 0
	for _, r := range results {
		totalLen += len(r)
	}

	all := make([]Fingerprint, totalLen)
	offset := 0
	for _, r := range results {
		copy(all[offset:], r)
		offset += len(r)
	}

	return all
}

func Process(ctx context.Context, samples []float64) []Fingerprint {
	sg := buildSpectrogram(ctx, samples)
	peaks := findPeaks(ctx, sg)
	return generateHashEntries(ctx, peaks)
}

func ProcessWithPeaks(ctx context.Context, samples []float64) ([]Fingerprint, [][]ConstellationPoint) {
	sg := buildSpectrogram(ctx, samples)
	rawPeaks := findPeaks(ctx, sg)

	var cleanPeaks [][]ConstellationPoint
	for _, framePeaks := range rawPeaks {
		if len(framePeaks) > 0 {
			cleanPeaks = append(cleanPeaks, framePeaks)
		}
	}

	return generateHashEntries(ctx, rawPeaks), cleanPeaks
}

func ProcessWithMetrics(ctx context.Context, samples []float64) ([]Fingerprint, StageMetrics) {
	var metrics StageMetrics
	totalStart := time.Now()

	t0 := time.Now()
	sg := buildSpectrogram(ctx, samples)
	metrics.SpectrogramDuration = time.Since(t0)

	t1 := time.Now()
	peaks := findPeaks(ctx, sg)
	metrics.PeakFindDuration = time.Since(t1)

	t2 := time.Now()
	entries := generateHashEntries(ctx, peaks)
	metrics.HashGenDuration = time.Since(t2)

	metrics.TotalDuration = time.Since(totalStart)
	return entries, metrics
}

// func MatchFingerprints(queryFps []Fingerprint, db map[uint64][]DBEntry) (string, uint64, float64, float64) {
// 	hist := make(map[OffsetKey]uint64)
// 	songTotals := make(map[string]uint64)

// 	for _, qfp := range queryFps {
// 		dbEntries := db[qfp.Hash]
// 		if len(dbEntries) == 0 {
// 			continue
// 		}

// 		for _, de := range dbEntries {
// 			rawOffset := de.AnchorTime - qfp.AnchorTime
// 			bin := uint64(math.Round(rawOffset / float64(HOP_SIZE)))

// 			key := OffsetKey{
// 				SongID: de.SongID,
// 				Bin:    bin,
// 			}

// 			hist[key]++
// 			songTotals[de.SongID]++
// 		}
// 	}

// 	// Early exit if no matching fingerprints were found
// 	if len(songTotals) == 0 {
// 		return "", 0, 0.0, 0.0
// 	}

// 	// 1. Find the most common track (SongID) overall
// 	var bestSongID string
// 	var maxSongTotal uint64
// 	for songID, total := range songTotals {
// 		if total > maxSongTotal {
// 			maxSongTotal = total
// 			bestSongID = songID
// 		}
// 	}

// 	// 2. Find the most common bin combo for the winning SongID
// 	var bestBin uint64
// 	var bestScore uint64
// 	var secondBestScore uint64

// 	for key, count := range hist {
// 		if key.SongID == bestSongID {
// 			if count > bestScore {
// 				secondBestScore = bestScore
// 				bestScore = count
// 				bestBin = key.Bin
// 			} else if count > secondBestScore {
// 				secondBestScore = count
// 			}
// 		}
// 	}

// 	// 3. Compute confidence ratio safely to prevent zero-division or index panics
// 	var confidenceRatio float64
// 	if secondBestScore > 0 {
// 		confidenceRatio = float64(bestScore) / float64(secondBestScore)
// 	} else {
// 		confidenceRatio = float64(bestScore)
// 	}

// 	timeOffset := float64(bestBin) * float64(HOP_SIZE) / float64(SAMPLE_RATE)
// 	return bestSongID, bestScore, timeOffset, confidenceRatio
// }

func MatchFingerprints(queryFps []Fingerprint, db map[uint64][]DBEntry) (string, uint64, float64, float64) {
	hist := make(map[OffsetKey]uint64)
	songTotals := make(map[string]uint64)

	for _, qfp := range queryFps {
		dbEntries := db[qfp.Hash]
		if len(dbEntries) == 0 {
			continue
		}

		for _, de := range dbEntries {
			rawOffset := de.AnchorTime - qfp.AnchorTime
			bin := uint64(math.Round(rawOffset / float64(HOP_SIZE)))

			key := OffsetKey{
				SongID: de.SongID,
				Bin:    bin,
			}

			hist[key]++
			songTotals[de.SongID]++
		}
	}

	// Early exit if no matching fingerprints were found
	if len(songTotals) == 0 {
		return "", 0, 0.0, 0.0
	}

	// 1. Find the 1st and 2nd choice tracks (SongID) by overall vote count
	var bestSongID string
	var bestSongTotal uint64
	var secondBestSongTotal uint64

	for songID, total := range songTotals {
		if total > bestSongTotal {
			secondBestSongTotal = bestSongTotal
			bestSongTotal = total
			bestSongID = songID
		} else if total > secondBestSongTotal {
			secondBestSongTotal = total
		}
	}

	// 2. Find the most common bin combo for the winning SongID
	var bestBin uint64
	var bestScore uint64

	for key, count := range hist {
		if key.SongID == bestSongID {
			if count > bestScore {
				bestScore = count
				bestBin = key.Bin
			}
		}
	}

	// 3. Compute confidence ratio (1st choice track votes / 2nd choice track votes)
	var confidenceRatio float64
	if secondBestSongTotal > 0 {
		confidenceRatio = float64(bestSongTotal) / float64(secondBestSongTotal)
	} else {
		confidenceRatio = float64(bestSongTotal)
	}

	timeOffset := float64(bestBin) * float64(HOP_SIZE) / float64(SAMPLE_RATE)
	return bestSongID, bestScore, timeOffset, confidenceRatio
}