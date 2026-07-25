package SpectralSpyOLD

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

type AudioChunk struct {
	Index     int
	Timestamp float64
	Samples   []float64
}

type FrameChunk struct {
	Index     int
	Timestamp float64
	Mags      []float64
}

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

type HashEntryWorkerResult struct {
	WorkerID	int
	Entries		[]HashEntry
}

type ChunkWorkerResult struct {
	WorkerID	int
	Chunks		[]AudioChunk
}

func chunkWorker(ctx context.Context, samples []float64) <-chan AudioChunk {
	out := make(chan AudioChunk)
	numSamples := len(samples)

	if numSamples == 0 {
		close(out)
		return out
	}

	numWorkers := runtime.GOMAXPROCS(0)
	
	// CRITICAL FIX: Divide by frames (windows), not raw sample lengths!
	numFrames := (numSamples + HOP_SIZE - 1) / HOP_SIZE
	framesPerWorker := (numFrames + numWorkers - 1) / numWorkers

	results := make(chan ChunkWorkerResult, numWorkers)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		startFrame := w * framesPerWorker
		endFrame := startFrame + framesPerWorker
		
		if endFrame > numFrames {
			endFrame = numFrames
		}
		if startFrame >= endFrame {
			break
		}

		wg.Add(1)
		go func(start, end, workerID int) {
			defer wg.Done()
			local := make([]AudioChunk, 0, end-start)

			for frame := start; frame < end; frame++ {
				// FIX 1: Stop crunching numbers if context is cancelled
				if ctx.Err() != nil {
					return 
				}

				i := frame * HOP_SIZE
				
				windowEnd := i + WINDOW_SIZE
				if windowEnd > numSamples {
					windowEnd = numSamples
				}
				
				chunk := make([]float64, WINDOW_SIZE)
				for j := i; j < windowEnd; j++ {
					idx := j - i
					chunk[idx] = samples[j] * HANN_WINDOW[idx]
				}

				local = append(local, AudioChunk{
					Index:     i,
					Timestamp: float64(i) + float64(WINDOW_SIZE)/2,
					Samples:   chunk,
				})
			}

			results <- ChunkWorkerResult{WorkerID: workerID, Chunks: local}
		}(startFrame, endFrame, w)
	}

	// Background goroutine to collect results and stream them out in order
	go func() {
		// FIX 2: Guarantee the channel is closed no matter how this function exits
		defer close(out) 
		
		wg.Wait()
		close(results)

		buffer := make(map[int][]AudioChunk)
		for r := range results {
			buffer[r.WorkerID] = r.Chunks
		}

		for i := 0; i < numWorkers; i++ {
			if chunks, ok := buffer[i]; ok {
				for _, c := range chunks {
					select {
					case <-ctx.Done():
						return // Now safe because of `defer close(out)`
					case out <- c:
					}
				}
			}
		}
	}()

	return out
}

// add multicore
func fftWorker(ctx context.Context, chunks <-chan AudioChunk) <-chan FrameChunk {
	out := make(chan FrameChunk)
	numWorkers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			
			// Each worker gets its own FFT plan to prevent race conditions
			fft := fourier.NewFFT(WINDOW_SIZE)

			for chunk := range chunks {
				// Exit early if cancelled
				if ctx.Err() != nil {
					return
				}

				coeffs := fft.Coefficients(nil, chunk.Samples[:WINDOW_SIZE])
				mags := make([]float64, len(coeffs)/2)

				for k := range mags {
					re := real(coeffs[k])
					im := imag(coeffs[k])
					mags[k] = re*re + im*im
				}

				select {
				case <-ctx.Done():
					return
				case out <- FrameChunk{
					Index:     chunk.Index, // We must pass the original index
					Timestamp: chunk.Timestamp,
					Mags:      mags,
				}:
				}
			}
		}()
	}

	// Close the channel once all workers are done
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func buildSpectrogram(ctx context.Context, frames <-chan FrameChunk) SpectrogramData {
	var mags [][]float64
	var times []float64

	for frame := range frames {
		if ctx.Err() != nil {
			break
		}

		// Calculate the true chronological position of the frame
		frameIdx := frame.Index / HOP_SIZE

		// Dynamically expand the slices if this frame is further ahead 
		// than our current allocated capacity
		for len(mags) <= frameIdx {
			mags = append(mags, nil)
			times = append(times, 0)
		}

		// Insert directly into the correct slot, regardless of arrival order
		mags[frameIdx] = frame.Mags
		times[frameIdx] = frame.Timestamp
	}

	return SpectrogramData{
		Mags:  mags,
		Times: times,
		BinHz: float64(SAMPLE_RATE) / float64(WINDOW_SIZE),
	}
}

// add multicore processing
func findPeaks(sg SpectrogramData) [][]ConstellationPoint {
	numFrames := len(sg.Mags)
	if numFrames == 0 {
		return nil
	}

	numBins := len(sg.Mags[0])
	peaks := make([][]ConstellationPoint, numFrames)

	for t := range numFrames {
		for f := range numBins {
			mag := sg.Mags[t][f]
			if mag == 0 {
				continue
			}

			isMax := true
			outer:
				for dt := -PEAK_NEIGHBORHOOD_TIME; dt <= PEAK_NEIGHBORHOOD_TIME; dt++ {
					nt := t + dt
					if nt < 0 || nt >= numFrames {
						continue
					}
					for df := -PEAK_NEIGHBORHOOD_FREQ; df <= PEAK_NEIGHBORHOOD_FREQ; df++ {
						nf := f + df
						if nf < 0 || nf >= numBins {
							continue
						}
						if sg.Mags[nt][nf] > mag {
							isMax = false
							break outer
						}
					}
				}
				if isMax {
					peaks[t] = append(peaks[t], ConstellationPoint{
						Timestamp: sg.Times[t],
						Frequency: float64(f) * sg.BinHz,
						Magnitude: mag,
						BinIndex:  f,
					})
				}
			}
	}
	
	return peaks
}

func hashPair(anchor, tether ConstellationPoint) uint64 {
	var buf [24]byte

	binary.LittleEndian.PutUint64(buf[0:8], math.Float64bits(anchor.Frequency))
	binary.LittleEndian.PutUint64(buf[8:16], math.Float64bits(tether.Frequency))
	binary.LittleEndian.PutUint64(buf[16:24], math.Float64bits(tether.Timestamp-anchor.Timestamp))

	return xxh3.Hash(buf[:])
}

func generateHashEntries(peaks [][]ConstellationPoint) []HashEntry {
	numFrames := len(peaks)
	if numFrames == 0 {
		return nil
	}

	numWorkers := runtime.GOMAXPROCS(0)
	chunkSize  := (numFrames + numWorkers - 1) / numWorkers

	results := make(chan HashEntryWorkerResult, numWorkers)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		end := min(start + chunkSize, numFrames)
		if start >= end {
			break
		}

		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			local := make([]HashEntry, 0, 512)

			for anchorT := start; anchorT < end; anchorT++ {
				for _, anchor := range peaks[anchorT] {
					for dt := TARGET_ZONE_TIME_START; dt <= TARGET_ZONE_TIME_END; dt++ {
						tetherT := anchorT + dt
						if tetherT >= numFrames {
							break
						}
						for _, tether := range peaks[tetherT] {
							binDelta := tether.BinIndex - anchor.BinIndex
							if binDelta < -TARGET_ZONE_FREQ_BINS || binDelta > TARGET_ZONE_FREQ_BINS {
								continue
							}
							local = append(local, HashEntry{
								Hash:       	hashPair(anchor, tether),
								AnchorTime: 	anchor.Timestamp,
							})
						}
					}
				}
			}

			results <- HashEntryWorkerResult{WorkerID: start, Entries: local}
		}(start, end)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	buffer := make(map[int][]HashEntry, numWorkers)
	for r := range results {
		buffer[r.WorkerID] = r.Entries
	}

	all := make([]HashEntry, 0)
	for i := 0; i < numFrames; i += chunkSize {
		if e, ok := buffer[i]; ok {
			all = append(all, e...)
		}
	}

	return all
}

func Process(ctx context.Context, samples []float64) []HashEntry {
	chunks := chunkWorker(ctx, samples)
	frames := fftWorker(ctx, chunks)
	sg     := buildSpectrogram(ctx, frames)

	fmt.Println("Spectogram Built")

	peaks  := findPeaks(sg)
	return generateHashEntries(peaks)
}

func ProcessWithPeaks(ctx context.Context, samples []float64) ([]HashEntry, [][]ConstellationPoint) {
	chunks := chunkWorker(ctx, samples)
	frames := fftWorker(ctx, chunks)
	sg     := buildSpectrogram(ctx, frames)

	rawPeaks := findPeaks(sg)
	
	var cleanPeaks [][]ConstellationPoint
	for _, framePeaks := range rawPeaks {
		if len(framePeaks) > 0 {
			cleanPeaks = append(cleanPeaks, framePeaks)
		}
	}

	return generateHashEntries(rawPeaks), cleanPeaks
}