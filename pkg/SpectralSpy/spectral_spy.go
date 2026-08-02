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
const HOP_SIZE    = WINDOW_SIZE / 2

const PEAK_NEIGHBORHOOD_TIME = 5 // ±frames
const PEAK_NEIGHBORHOOD_FREQ = 5 // ±FFT bins

const TARGET_ZONE_TIME_START = 1  // frames ahead where zone begins
const TARGET_ZONE_TIME_END   = 18 // frames ahead where zone ends
const TARGET_ZONE_FREQ_BINS  = 13 // ±FFT bins around anchor frequency bin
const MAX_FAN_OUT            = 10

const MAX_PEAKS_PER_FRAME = 4  // Top-K frame-level rate limit
const CFAR_TRAIN_BINS    = 8  // CFAR noise estimation window size
const CFAR_GUARD_BINS    = 2  // CFAR guard window size
const CFAR_FACTOR        = 2.5 // SNR factor over dynamic noise floor

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

type ConstellationPoint struct {
	Timestamp float64
	Frequency float64
	Magnitude float64
	BinIndex  int
}

type HashEntry struct {
	Hash       uint64
	AnchorTime float64
}

type SpectrogramData struct {
	Mags  [][]float64
	Times []float64
	BinHz float64
}

type StageMetrics struct {
	SpectrogramDuration time.Duration `json:"spectrogram_duration"`
	PeakFindDuration    time.Duration `json:"peak_find_duration"`
	HashGenDuration     time.Duration `json:"hash_gen_duration"`
	TotalDuration       time.Duration `json:"total_duration"`
}

func buildSpectrogram(ctx context.Context, samples []float64) SpectrogramData {
	numSamples := len(samples)
	if numSamples == 0 {
		return SpectrogramData{BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE)}
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

	return SpectrogramData{
		Mags:  mags,
		Times: times,
		BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE),
	}
}

func findPeaks(ctx context.Context, sg SpectrogramData) [][]ConstellationPoint {
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

					cMin := max(0, f-CFAR_TRAIN_BINS)
					cMax := min(numBins-1, f+CFAR_TRAIN_BINS)
					noiseSum := 0.0
					noiseCount := 0

					for nf := cMin; nf <= cMax; nf++ {
						if nf < f-CFAR_GUARD_BINS || nf > f+CFAR_GUARD_BINS {
							noiseSum += sg.Mags[t][nf]
							noiseCount++
						}
					}

					if noiseCount > 0 {
						noiseFloor := noiseSum / float64(noiseCount)
						if mag < noiseFloor*CFAR_FACTOR {
							continue // reject signal below local dynamic noise floor
						}
					}

					// local maxima check
					fMin := max(0, f-PEAK_NEIGHBORHOOD_FREQ)
					fMax := min(numBins-1, f+PEAK_NEIGHBORHOOD_FREQ)

					isMax := true
					for nt := tMin; nt <= tMax && isMax; nt++ {
						for nf := fMin; nf <= fMax; nf++ {
							if sg.Mags[nt][nf] > mag {
								isMax = false
								break
							}
						}
					}

					if isMax {
						localPeaks = append(localPeaks, ConstellationPoint{
							Timestamp: sg.Times[t],
							Frequency: float64(f) * sg.BinHz,
							Magnitude: mag,
							BinIndex:  f,
						})
					}
				}

				// top-K frame level rate limiting
				if len(localPeaks) > MAX_PEAKS_PER_FRAME {
					// sort by magnitude descending to select top K
					sort.Slice(localPeaks, func(i, j int) bool {
						return localPeaks[i].Magnitude > localPeaks[j].Magnitude
					})
					localPeaks = localPeaks[:MAX_PEAKS_PER_FRAME]

					// sort ascending by BinIndex
					sort.Slice(localPeaks, func(i, j int) bool {
						return localPeaks[i].BinIndex < localPeaks[j].BinIndex
					})
				}

				if len(localPeaks) > 0 {
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

func generateHashEntries(ctx context.Context, peaks [][]ConstellationPoint) []HashEntry {
	numFrames := len(peaks)
	if numFrames == 0 {
		return nil
	}

	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	results := make([][]HashEntry, numWorkers)
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

			local := make([]HashEntry, 0, 1024)

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

							local = append(local, HashEntry{
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

	all := make([]HashEntry, totalLen)
	offset := 0
	for _, r := range results {
		copy(all[offset:], r)
		offset += len(r)
	}

	return all
}

func Process(ctx context.Context, samples []float64) []HashEntry {
	sg := buildSpectrogram(ctx, samples)
	peaks := findPeaks(ctx, sg)
	return generateHashEntries(ctx, peaks)
}

func ProcessWithPeaks(ctx context.Context, samples []float64) ([]HashEntry, [][]ConstellationPoint) {
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

func ProcessWithMetrics(ctx context.Context, samples []float64) ([]HashEntry, StageMetrics) {
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

func generateHashEntriesWithParams(ctx context.Context, peaks [][]ConstellationPoint, freqBins, timeStart, timeEnd int) []HashEntry {
	numFrames := len(peaks)
	if numFrames == 0 {
		return nil
	}

	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	results := make([][]HashEntry, numWorkers)
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

			local := make([]HashEntry, 0, 1024)

			for anchorT := start; anchorT < end; anchorT++ {
				if ctx.Err() != nil {
					return
				}

				tetherStart := anchorT + timeStart
				tetherEnd := min(anchorT+timeEnd, numFrames-1)

				for _, anchor := range peaks[anchorT] {
					minBin := anchor.BinIndex - freqBins
					maxBin := anchor.BinIndex + freqBins

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

							local = append(local, HashEntry{
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

	all := make([]HashEntry, totalLen)
	offset := 0
	for _, r := range results {
		copy(all[offset:], r)
		offset += len(r)
	}

	return all
}

func ProcessWithParams(ctx context.Context, samples []float64, freqBins, timeStart, timeEnd int) ([]HashEntry, StageMetrics) {
	if freqBins < 1 {
		freqBins = 1
	} else if freqBins > 32 {
		freqBins = 32
	}

	if timeStart < 1 {
		timeStart = 1
	}

	if timeEnd <= timeStart {
		timeEnd = timeStart + 1
	} else if timeEnd > 200 {
		timeEnd = 200
	}

	var metrics StageMetrics
	totalStart := time.Now()

	t0 := time.Now()
	sg := buildSpectrogram(ctx, samples)
	metrics.SpectrogramDuration = time.Since(t0)

	t1 := time.Now()
	peaks := findPeaks(ctx, sg)
	metrics.PeakFindDuration = time.Since(t1)

	t2 := time.Now()
	entries := generateHashEntriesWithParams(ctx, peaks, freqBins, timeStart, timeEnd)
	metrics.HashGenDuration = time.Since(t2)

	metrics.TotalDuration = time.Since(totalStart)
	return entries, metrics
}