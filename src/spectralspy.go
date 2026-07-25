package SpectralSpy

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"

	"github.com/zeebo/xxh3"
	"gonum.org/v1/gonum/dsp/fourier"
)

const SAMPLE_RATE = 44100
const WINDOW_SIZE = 4096
const HOP_SIZE    = WINDOW_SIZE / 2

const PEAK_NEIGHBORHOOD_TIME = 5 // ±frames
const PEAK_NEIGHBORHOOD_FREQ = 5 // ±FFT bins

const TARGET_ZONE_TIME_START = 2  // frames ahead where zone begins
const TARGET_ZONE_TIME_END   = 7  // frames ahead where zone ends
const TARGET_ZONE_FREQ_BINS  = 10 // ±FFT bins around anchor frequency bin

var HANN_WINDOW = func() []float64 {
	w := make([]float64, WINDOW_SIZE)
	for n := range WINDOW_SIZE {
		w[n] = 0.5 * (1 - math.Cos(2*math.Pi*float64(n)/float64(WINDOW_SIZE-1)))
	}
	return w
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

func buildSpectrogram(ctx context.Context, samples []float64) SpectrogramData {
	numSamples := len(samples)
	if numSamples == 0 {
		return SpectrogramData{BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE)}
	}

	// pre-calculate exact array sizes
	numFrames := (numSamples + HOP_SIZE - 1) / HOP_SIZE
	mags := make([][]float64, numFrames)
	times := make([]float64, numFrames)

	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		// allocate fft struct per worker to prevent race conditions
		fft := fourier.NewFFT(WINDOW_SIZE)

		startFrame := w * framesPerWorker
		endFrame := min((startFrame + framesPerWorker), numFrames)

		if startFrame >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			// allocate worker buffer
			chunk := make([]float64, WINDOW_SIZE)

			for frame := start; frame < end; frame++ {
				if ctx.Err() != nil {
					return // return if context is cancelled
				}

				i := frame * HOP_SIZE

				for j := 0; j < WINDOW_SIZE; j++ {
					if i+j < numSamples {
						chunk[j] = samples[i+j] * HANN_WINDOW[j] // apply hann-window to samples
					} else {
						chunk[j] = 0.0 // zero padding
					}
				}

				// calculate coefficients
				coeffs := fft.Coefficients(nil, chunk)
				magSlice := make([]float64, len(coeffs)/2) // time 

				for k := range magSlice {
					re := real(coeffs[k])
					im := imag(coeffs[k])
					magSlice[k] = re*re + im*im
				}

				// Write directly into the pre-allocated slice without locking
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

	// pre-allocate peaks slice
	peaks := make([][]ConstellationPoint, numFrames)
	numWorkers := runtime.GOMAXPROCS(0)
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers
	
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		startFrame := w * framesPerWorker
		endFrame := min((startFrame + framesPerWorker),numFrames)

		if startFrame >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()

			for t := start; t < end; t++ {
				if ctx.Err() != nil {
					return // exit if context is cancelled
				}
			
				var localPeaks []ConstellationPoint
				
				// clamp time boundaries once per frame
				tMin := max(0, t-PEAK_NEIGHBORHOOD_TIME)
				tMax := min(numFrames-1, t+PEAK_NEIGHBORHOOD_TIME)
			
				for f := 0; f < numBins; f++ {
					mag := sg.Mags[t][f]
					if mag == 0 {
						continue // skip empty magnitudes
					}
			
					// clamp frequency boundaries once per frequency bin
					fMin := max(0, f-PEAK_NEIGHBORHOOD_FREQ)
					fMax := min(numBins-1, f+PEAK_NEIGHBORHOOD_FREQ)
			
					isMax := true
					
					// loop over clamped sliding time window
					for nt := tMin; nt <= tMax && isMax; nt++ {
						for nf := fMin; nf <= fMax; nf++ {
							if sg.Mags[nt][nf] > mag {
								isMax = false
								mag = sg.Mags[nt][nf]
								break // terminate inside loop on counterexample of larger magnitude
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

	// add indentifying features including frequencies and timestamps
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
		end := min((start + framesPerWorker), numFrames)
		
		if start >= numFrames {
			break
		}

		wg.Add(1)
		go func(start, end, workerID int) {
			defer wg.Done()
			
			// pre-allocate array to prevent reallocation ops.
			local := make([]HashEntry, 0, 1024)

			for anchorT := start; anchorT < end; anchorT++ {
				if ctx.Err() != nil {
					return
				}
				
				// clamp time boundaries per anchor frame
				tetherStart := anchorT + TARGET_ZONE_TIME_START
				tetherEnd := min(anchorT+TARGET_ZONE_TIME_END, numFrames-1)
				
				for _, anchor := range peaks[anchorT] {
					// clamp frequency bin range
					minBin := anchor.BinIndex - TARGET_ZONE_FREQ_BINS
					maxBin := anchor.BinIndex + TARGET_ZONE_FREQ_BINS
					
					for tetherT := tetherStart; tetherT <= tetherEnd; tetherT++ {
						for _, tether := range peaks[tetherT] {
							
							// since peaks are sorted in order, we can use filters
							if tether.BinIndex < minBin {
								continue // not in the target zone yet, keep looking
							}
							if tether.BinIndex > maxBin {
								break // we have passed the target zone. stop checking the rest of this frame
							}
							
							local = append(local, HashEntry{
								Hash:       hashPair(anchor, tether),
								AnchorTime: anchor.Timestamp,
							})
						}
					}
				}
			}
			results[workerID] = local
		}(start, end, w)
	}

	wg.Wait()

	// consolidate the array of slices into a single flat array
	var totalLen int
	for _, r := range results {
		totalLen += len(r)
	}

	// pre-allocate final slice
	all := make([]HashEntry, totalLen)

	var offset int
	for _, r := range results {
		copy(all[offset:], r) // copy() writes directly to memory without capacity checks
		offset += len(r)
	}

	return all
}

func Process(ctx context.Context, samples []float64) []HashEntry {
	sg := buildSpectrogram(ctx, samples)
	fmt.Println("Spectrogram Built")
	
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