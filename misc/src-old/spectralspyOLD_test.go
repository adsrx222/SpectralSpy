package SpectralSpyOLD

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

// helpers

func makeSilence(n int) []float64 { return make([]float64, n) }

func makeSine(n int, freq float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Sin(2 * math.Pi * freq * float64(i) / SAMPLE_RATE)
	}
	return out
}

func drainChunks(ch <-chan AudioChunk) []AudioChunk {
	var out []AudioChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

func drainFrames(ch <-chan FrameChunk) []FrameChunk {
	var out []FrameChunk
	for f := range ch {
		out = append(out, f)
	}
	return out
}

func loadMP3(path string) ([]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	decoder, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(decoder)
	if err != nil {
		return nil, err
	}

	samples := make([]float64, 0, len(data)/4)
	for i := 0; i+3 < len(data); i += 4 {
		left := int16(uint16(data[i]) | uint16(data[i+1])<<8)
		right := int16(uint16(data[i+2]) | uint16(data[i+3])<<8)
		mono := (float64(left) + float64(right)) / 2.0
		samples = append(samples, mono/32768.0)
	}
	return samples, nil
}

func addNoise(noiseLevel float64, samples []float64, rng *rand.Rand) []float64 {
	peak := 0.0
	for _, s := range samples {
		if a := math.Abs(s); a > peak {
			peak = a
		}
	}
	if peak == 0 {
		peak = 1.0
	}
	scale := noiseLevel * peak
	noisy := make([]float64, len(samples))
	for i, s := range samples {
		u1 := 1.0 - rng.Float64()
		u2 := rng.Float64()
		gauss := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
		noisy[i] = s + gauss*scale
	}
	return noisy
}

func hashesFrom(entries []HashEntry) []uint64 {
	out := make([]uint64, len(entries))
	for i, e := range entries {
		out[i] = e.Hash
	}
	return out
}

func hashSet(entries []HashEntry) map[uint64]struct{} {
	m := make(map[uint64]struct{}, len(entries))
	for _, e := range entries {
		m[e.Hash] = struct{}{}
	}
	return m
}

// chunkWorker

func TestChunkWorker_ChannelClosesAfterDrain(t *testing.T) {
	ctx := context.Background()
	ch := chunkWorker(ctx, makeSilence(WINDOW_SIZE*3))

	done := make(chan struct{})
	go func() { drainChunks(ch); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("chunkWorker channel never closed")
	}
}

func TestChunkWorker_ChunkCount(t *testing.T) {
	n := WINDOW_SIZE * 4
	expected := (n + HOP_SIZE - 1) / HOP_SIZE
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(n)))
	if len(chunks) != expected {
		t.Errorf("expected %d chunks, got %d", expected, len(chunks))
	}
}

func TestChunkWorker_ChunkLength(t *testing.T) {
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(WINDOW_SIZE*2)))
	for i, c := range chunks {
		if len(c.Samples) != WINDOW_SIZE {
			t.Errorf("chunk %d: expected length %d, got %d", i, WINDOW_SIZE, len(c.Samples))
		}
	}
}

func TestChunkWorker_TimestampPositive(t *testing.T) {
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(WINDOW_SIZE*2)))
	for i, c := range chunks {
		if c.Timestamp < 0 {
			t.Errorf("chunk %d: negative timestamp %f", i, c.Timestamp)
		}
	}
}

func TestChunkWorker_TimestampIsWindowCentre(t *testing.T) {
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(WINDOW_SIZE*4)))
	for i, c := range chunks {
		sampleOffset := float64(i * HOP_SIZE)
		want := sampleOffset + float64(WINDOW_SIZE)/2
		if c.Timestamp != want {
			t.Errorf("chunk %d: timestamp = %f, want %f", i, c.Timestamp, want)
		}
	}
}

func TestChunkWorker_HannWindowApplied(t *testing.T) {
	ones := make([]float64, WINDOW_SIZE)
	for i := range ones {
		ones[i] = 1.0
	}
	chunks := drainChunks(chunkWorker(context.Background(), ones))
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	if chunks[0].Samples[0] != 0.0 {
		t.Errorf("expected Hann-windowed first sample to be 0, got %f", chunks[0].Samples[0])
	}
}

func TestChunkWorker_CancellationStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := chunkWorker(ctx, makeSilence(SAMPLE_RATE*60))
	<-ch
	cancel()

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("chunkWorker did not respect context cancellation")
	}
}

func TestChunkWorker_EmptyInput(t *testing.T) {
	chunks := drainChunks(chunkWorker(context.Background(), []float64{}))
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty input, got %d", len(chunks))
	}
}

func TestChunkWorker_ShortInput(t *testing.T) {
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(WINDOW_SIZE/2)))
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for short input, got %d", len(chunks))
	}
}

// fftWorker

func fftPipeline(ctx context.Context, samples []float64) []FrameChunk {
	return drainFrames(fftWorker(ctx, chunkWorker(ctx, samples)))
}

func TestFFTWorker_FrameCount(t *testing.T) {
	n := WINDOW_SIZE * 4
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(n)))
	frames := fftPipeline(context.Background(), makeSilence(n))
	if len(frames) != len(chunks) {
		t.Errorf("expected %d frames, got %d", len(chunks), len(frames))
	}
}

func TestFFTWorker_MagsLength(t *testing.T) {
	frames := fftPipeline(context.Background(), makeSilence(WINDOW_SIZE*2))
	for i, f := range frames {
		if len(f.Mags) == 0 {
			t.Errorf("frame %d: empty magnitude slice", i)
		}
	}
}

func TestFFTWorker_SilenceIsZero(t *testing.T) {
	frames := fftPipeline(context.Background(), makeSilence(WINDOW_SIZE*2))
	for i, f := range frames {
		for k, m := range f.Mags {
			if m != 0.0 {
				t.Errorf("frame %d bin %d: expected 0 for silence, got %f", i, k, m)
				return
			}
		}
	}
}

func TestFFTWorker_MagsNonNegative(t *testing.T) {
	frames := fftPipeline(context.Background(), makeSine(WINDOW_SIZE*4, 440))
	for i, f := range frames {
		for k, m := range f.Mags {
			if m < 0 {
				t.Errorf("frame %d bin %d: negative magnitude %f", i, k, m)
			}
		}
	}
}

func TestFFTWorker_SineHasDominantBin(t *testing.T) {
	freq := 1000.0
	frames := fftPipeline(context.Background(), makeSine(WINDOW_SIZE*4, freq))
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	f := frames[0]
	maxIdx, maxVal := 0, 0.0
	for i, m := range f.Mags {
		if m > maxVal {
			maxVal = m
			maxIdx = i
		}
	}
	fftBinHz := float64(SAMPLE_RATE) / float64(WINDOW_SIZE)
	peakFreq := float64(maxIdx) * fftBinHz
	if math.Abs(peakFreq-freq) > 200 {
		t.Errorf("dominant bin at %.1f Hz, expected near %.1f Hz", peakFreq, freq)
	}
}

func TestFFTWorker_CancellationStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := chunkWorker(ctx, makeSilence(SAMPLE_RATE*60))
	frames := fftWorker(ctx, ch)
	<-frames
	cancel()

	done := make(chan struct{})
	go func() {
		for range frames {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fftWorker did not respect context cancellation")
	}
}

// // // ─────────────────────────────────────────────
// // // buildSpectrogram
// // // ─────────────────────────────────────────────

func TestBuildSpectrogram_FrameCount(t *testing.T) {
	ctx := context.Background()
	n := WINDOW_SIZE * 8
	frames := fftWorker(ctx, chunkWorker(ctx, makeSilence(n)))
	sg := buildSpectrogram(ctx, frames)

	expectedFrames := (n + HOP_SIZE - 1) / HOP_SIZE
	if len(sg.Mags) != expectedFrames {
		t.Errorf("expected %d frames in spectrogram, got %d", expectedFrames, len(sg.Mags))
	}
}

func TestBuildSpectrogram_TimestampsMatchChunks(t *testing.T) {
	ctx := context.Background()
	n := WINDOW_SIZE * 4
	chunks := drainChunks(chunkWorker(context.Background(), makeSilence(n)))

	frames := fftWorker(ctx, chunkWorker(ctx, makeSilence(n)))
	sg := buildSpectrogram(ctx, frames)

	if len(sg.Times) != len(chunks) {
		t.Fatalf("timestamp count %d != chunk count %d", len(sg.Times), len(chunks))
	}
	for i, c := range chunks {
		if sg.Times[i] != c.Timestamp {
			t.Errorf("frame %d: sg.times=%f chunk.Timestamp=%f", i, sg.Times[i], c.Timestamp)
		}
	}
}

func TestBuildSpectrogram_BinHz(t *testing.T) {
	ctx := context.Background()
	frames := fftWorker(ctx, chunkWorker(ctx, makeSilence(WINDOW_SIZE*2)))
	sg := buildSpectrogram(ctx, frames)
	want := float64(SAMPLE_RATE) / float64(WINDOW_SIZE)
	if sg.BinHz != want {
		t.Errorf("binHz = %f, want %f", sg.BinHz, want)
	}
}

func TestBuildSpectrogram_SilenceAllZero(t *testing.T) {
	ctx := context.Background()
	frames := fftWorker(ctx, chunkWorker(ctx, makeSilence(WINDOW_SIZE*4)))
	sg := buildSpectrogram(ctx, frames)
	for t2, row := range sg.Mags {
		for b, m := range row {
			if m != 0 {
				t.Errorf("silence: sg.mags[%d][%d] = %f, want 0", t2, b, m)
				return
			}
		}
	}
}

// findPeaks

func makeSpectrogram(samples []float64) SpectrogramData {
	ctx := context.Background()
	frames := fftWorker(ctx, chunkWorker(ctx, samples))
	return buildSpectrogram(ctx, frames)
}

func TestFindPeaks_SilenceNoPeaks(t *testing.T) {
	sg := makeSpectrogram(makeSilence(WINDOW_SIZE * 8))
	peaks := findPeaks(sg)
	for i, row := range peaks {
		if len(row) > 0 {
			t.Errorf("frame %d: expected 0 peaks for silence, got %d", i, len(row))
			return
		}
	}
}

func TestFindPeaks_SineHasPeaks(t *testing.T) {
	sg := makeSpectrogram(makeSine(WINDOW_SIZE*16, 1000))
	peaks := findPeaks(sg)
	total := 0
	for _, row := range peaks {
		total += len(row)
	}
	if total == 0 {
		t.Error("expected peaks for a 1 kHz sine wave, got none")
	}
}

func TestFindPeaks_PeaksAreSparse(t *testing.T) {
	n := WINDOW_SIZE * 32
	sg := makeSpectrogram(makeSine(n, 440))
	peaks := findPeaks(sg)

	totalCells := len(sg.Mags) * len(sg.Mags[0])
	totalPeaks := 0
	for _, row := range peaks {
		totalPeaks += len(row)
	}
	if totalCells > 0 && float64(totalPeaks)/float64(totalCells) > 0.10 {
		t.Errorf("peaks are not sparse: %d peaks in %d cells (%.1f%%)",
			totalPeaks, totalCells, float64(totalPeaks)/float64(totalCells)*100)
	}
}

func TestFindPeaks_PeaksAreLocalMaxima(t *testing.T) {
	sg := makeSpectrogram(makeSine(WINDOW_SIZE*16, 800))
	peaks := findPeaks(sg)

	numFrames := len(sg.Mags)
	numBins := len(sg.Mags[0])

	for t2, row := range peaks {
		for _, p := range row {
			bin := int(math.Round(p.Frequency / sg.BinHz))
			mag := sg.Mags[t2][bin]

			for dt := -PEAK_NEIGHBORHOOD_TIME; dt <= PEAK_NEIGHBORHOOD_TIME; dt++ {
				nt := t2 + dt
				if nt < 0 || nt >= numFrames {
					continue
				}
				for df := -PEAK_NEIGHBORHOOD_FREQ; df <= PEAK_NEIGHBORHOOD_FREQ; df++ {
					nf := bin + df
					if nf < 0 || nf >= numBins {
						continue
					}
					if sg.Mags[nt][nf] > mag {
						t.Errorf("peak at (frame=%d, bin=%d) is not a local maximum: "+
							"neighbour (frame=%d, bin=%d) has higher magnitude",
							t2, bin, nt, nf)
						return
					}
				}
			}
		}
	}
}

func TestFindPeaks_NonNegativeMagnitudes(t *testing.T) {
	sg := makeSpectrogram(makeSine(WINDOW_SIZE*8, 440))
	for _, row := range findPeaks(sg) {
		for _, p := range row {
			if p.Magnitude < 0 {
				t.Errorf("negative magnitude in peak: %f", p.Magnitude)
			}
		}
	}
}

func TestFindPeaks_BinIndexConsistent(t *testing.T) {
	sg := makeSpectrogram(makeSine(WINDOW_SIZE*8, 1000))
	for _, row := range findPeaks(sg) {
		for _, p := range row {
			want := int(math.Round(p.Frequency / sg.BinHz))
			if p.BinIndex != want {
				t.Errorf("BinIndex=%d but Frequency/binHz=%.1f (want %d)",
					p.BinIndex, p.Frequency/sg.BinHz, want)
			}
		}
	}
}

// hashPair()

func makePoint(ts, freq, mag float64) ConstellationPoint {
	return ConstellationPoint{
		Timestamp: ts,
		Frequency: freq,
		Magnitude: mag,
		BinIndex:  int(math.Round(freq / (float64(SAMPLE_RATE) / float64(WINDOW_SIZE)))),
	}
}

func TestHashPair_Deterministic(t *testing.T) {
	a := makePoint(1.0, 440.0, 100.0)
	b := makePoint(2.0, 880.0, 200.0)
	if hashPair(a, b) != hashPair(a, b) {
		t.Error("hashPair is not deterministic")
	}
}

func TestHashPair_DifferentTethersDifferentHash(t *testing.T) {
	a := makePoint(1.0, 440.0, 100.0)
	b := makePoint(2.0, 880.0, 200.0)
	c := makePoint(3.0, 1760.0, 50.0)
	if hashPair(a, b) == hashPair(a, c) {
		t.Error("different tether points produced the same hash")
	}
}

func TestHashPair_NonZero(t *testing.T) {
	a := makePoint(1.0, 440.0, 1.0)
	b := makePoint(2.0, 880.0, 1.0)
	if hashPair(a, b) == 0 {
		t.Error("hash should not be zero for non-trivial inputs")
	}
}

func TestHashPair_FrequencyMatters(t *testing.T) {
	base := makePoint(0.0, 440.0, 1.0)
	same := makePoint(0.0, 440.0, 1.0)
	diff := makePoint(0.0, 880.0, 1.0)
	if hashPair(base, same) == hashPair(base, diff) {
		t.Error("different tether frequencies must produce different hashes")
	}
}

func TestHashPair_DeltaTimeMatters(t *testing.T) {
	anchor := makePoint(0.0, 440.0, 1.0)
	t1 := makePoint(1.0, 880.0, 1.0)
	t2 := makePoint(2.0, 880.0, 1.0)
	if hashPair(anchor, t1) == hashPair(anchor, t2) {
		t.Error("different time deltas must produce different hashes")
	}
}

func TestHashPair_PositionIndependent(t *testing.T) {
	anchor1 := makePoint(10.0, 440.0, 1.0)
	tether1 := makePoint(12.0, 880.0, 1.0)

	anchor2 := makePoint(50.0, 440.0, 1.0)
	tether2 := makePoint(52.0, 880.0, 1.0)

	if hashPair(anchor1, tether1) != hashPair(anchor2, tether2) {
		t.Error("pairs with same (f_A, f_B, ΔT) at different positions must hash identically")
	}
}

func TestHashPair_AbsolutePositionDoesNotMatter(t *testing.T) {
	const shift = 1000.0
	anchor := makePoint(5.0, 440.0, 1.0)
	tether := makePoint(7.0, 880.0, 1.0)

	shiftedAnchor := makePoint(5.0+shift, 440.0, 1.0)
	shiftedTether := makePoint(7.0+shift, 880.0, 1.0)

	if hashPair(anchor, tether) != hashPair(shiftedAnchor, shiftedTether) {
		t.Error("shifting both anchor and tether by a constant must not change hash")
	}
}

// generateHashEntries()

func makePeaks(numFrames, peaksPerFrame int, baseFreq, baseTime float64) [][]ConstellationPoint {
	binHz := float64(SAMPLE_RATE) / float64(WINDOW_SIZE)
	pm := make([][]ConstellationPoint, numFrames)
	for r := range pm {
		pm[r] = make([]ConstellationPoint, peaksPerFrame)
		for c := range pm[r] {
			freq := baseFreq + float64(c)*binHz*float64(TARGET_ZONE_FREQ_BINS)
			bin := int(math.Round(freq / binHz))
			pm[r][c] = ConstellationPoint{
				Timestamp: baseTime + float64(r),
				Frequency: freq,
				Magnitude: 1.0,
				BinIndex:  bin,
			}
		}
	}
	return pm
}

func TestGenerateHashEntries_EmptyInput(t *testing.T) {
	entries := generateHashEntries(nil)
	if entries != nil && len(entries) != 0 {
		t.Errorf("expected nil/empty for nil input, got %d entries", len(entries))
	}
}

func TestGenerateHashEntries_NonEmpty(t *testing.T) {
	peaks := makePeaks(TARGET_ZONE_TIME_END+2, 3, 440.0, 0.0)
	entries := generateHashEntries(peaks)
	if len(entries) == 0 {
		t.Error("expected hash entries for populated peaks, got none")
	}
}

func TestGenerateHashEntries_Deterministic(t *testing.T) {
	peaks := makePeaks(20, 4, 440.0, 0.0)
	e1 := generateHashEntries(peaks)
	e2 := generateHashEntries(peaks)
	if len(e1) != len(e2) {
		t.Fatalf("non-deterministic length: %d vs %d", len(e1), len(e2))
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Errorf("entry %d differs between runs: %+v vs %+v", i, e1[i], e2[i])
		}
	}
}

func TestGenerateHashEntries_AnchorTimePreserved(t *testing.T) {
	peaks := makePeaks(TARGET_ZONE_TIME_END+3, 2, 440.0, 100.0)
	entries := generateHashEntries(peaks)
	if len(entries) == 0 {
		t.Fatal("no entries produced")
	}
	validTimes := make(map[float64]struct{})
	for _, row := range peaks {
		for _, p := range row {
			validTimes[p.Timestamp] = struct{}{}
		}
	}
	for i, e := range entries {
		if _, ok := validTimes[e.AnchorTime]; !ok {
			t.Errorf("entry %d: AnchorTime %f not found in peak timestamps", i, e.AnchorTime)
			return
		}
	}
}

func TestGenerateHashEntries_OnlyForwardPairs(t *testing.T) {
	peaks := makePeaks(TARGET_ZONE_TIME_END+5, 2, 440.0, 0.0)
	maxAnchorTime := 0.0
	for _, row := range peaks {
		for _, p := range row {
			if p.Timestamp > maxAnchorTime {
				maxAnchorTime = p.Timestamp
			}
		}
	}
	for i, e := range generateHashEntries(peaks) {
		if e.AnchorTime > maxAnchorTime {
			t.Errorf("entry %d: AnchorTime %f > max peak time %f", i, e.AnchorTime, maxAnchorTime)
		}
	}
}

func TestGenerateHashEntries_NoBoundaryDuplication(t *testing.T) {
	peaks := makePeaks(40, 3, 440.0, 0.0)
	entries := generateHashEntries(peaks)

	type key struct {
		h uint64
		t float64
	}
	seen := make(map[key]int)
	for _, e := range entries {
		k := key{e.Hash, e.AnchorTime}
		seen[k]++
	}
	for k, count := range seen {
		if count > 1 {
			t.Errorf("duplicate entry: hash=%016x anchorTime=%f appears %d times",
				k.h, k.t, count)
			return
		}
	}
}

func TestGenerateHashEntries_DifferentPeaksDifferentHashes(t *testing.T) {
	p1 := makePeaks(TARGET_ZONE_TIME_END+2, 2, 440.0, 0.0)
	p2 := makePeaks(TARGET_ZONE_TIME_END+2, 2, 880.0, 0.0)
	h1 := hashesFrom(generateHashEntries(p1))
	h2 := hashesFrom(generateHashEntries(p2))

	set1 := make(map[uint64]struct{}, len(h1))
	for _, h := range h1 {
		set1[h] = struct{}{}
	}
	overlap := 0
	for _, h := range h2 {
		if _, ok := set1[h]; ok {
			overlap++
		}
	}
	if len(h1) > 0 && float64(overlap)/float64(len(h1)) > 0.5 {
		t.Errorf("too much overlap (%d/%d) between different peak maps", overlap, len(h1))
	}
}

// Process()

func TestProcess_ReturnsSomething(t *testing.T) {
	entries := Process(context.Background(), makeSine(SAMPLE_RATE*2, 440))
	if len(entries) == 0 {
		t.Error("Process returned no entries for a 2-second sine wave")
	}
}

func TestProcess_Deterministic(t *testing.T) {
	samples := makeSine(SAMPLE_RATE, 1000)
	e1 := Process(context.Background(), samples)
	e2 := Process(context.Background(), samples)
	if len(e1) != len(e2) {
		t.Fatalf("non-deterministic length: %d vs %d", len(e1), len(e2))
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Errorf("entry %d differs between runs", i)
		}
	}
}

func TestProcess_SilenceVsSine(t *testing.T) {
	n := SAMPLE_RATE * 2
	eSilence := Process(context.Background(), makeSilence(n))
	eSine := Process(context.Background(), makeSine(n, 440))

	if len(eSilence) != 0 {
		t.Errorf("expected 0 entries for silence, got %d", len(eSilence))
	}
	if len(eSine) == 0 {
		t.Error("expected entries for a sine wave, got none")
	}
}

func TestProcess_CancellationReturnsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		Process(ctx, makeSilence(SAMPLE_RATE*60))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Process did not return after context cancellation")
	}
}

func TestProcess_EmptyInput(t *testing.T) {
	entries := Process(context.Background(), []float64{})
	if entries == nil {
		return
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty input, got %d", len(entries))
	}
}

func TestProcess_AnchorTimesNonNegative(t *testing.T) {
	for _, e := range Process(context.Background(), makeSine(SAMPLE_RATE, 440)) {
		if e.AnchorTime < 0 {
			t.Errorf("negative AnchorTime: %f", e.AnchorTime)
			return
		}
	}
}

func TestProcess_PositionIndependentHashes(t *testing.T) {
	sine := makeSine(WINDOW_SIZE*32, 1000)
	
	seed := time.Now().UnixNano()
    rng := rand.New(rand.NewSource(seed))
    noisySine := addNoise(0.01, sine, rng)

	e1 := Process(context.Background(), noisySine)
	padded := append(makeSilence(WINDOW_SIZE*8), noisySine...)
	e2 := Process(context.Background(), padded)

	set1 := hashSet(e1)
	matches := 0
	for _, e := range e2 {
		if _, ok := set1[e.Hash]; ok {
			matches++
		}
	}
	if len(e1) == 0 {
		t.Fatal("no entries for clip 1")
	}
	pct := float64(matches) / float64(len(e1)) * 100
	if pct < 10.0 {
		t.Errorf("position-independence: only %.1f%% of hashes matched across offsets", pct)
	}
}

func TestGenerateHashes(t *testing.T) {
	samples, err := loadMP3("../testdata/001.mp3")
	if err != nil {
		t.Fatalf("failed to load mp3: %v", err)
	}

	entries := Process(context.Background(), samples)
	if len(entries) == 0 {
		t.Error("Process returned no entries for 001.mp3")
	}

	fmt.Printf("\nGenerated %d hash entries\n", len(entries))
	limit := min(200, len(entries))
	for i := 0; i < limit; i++ {
		fmt.Printf("%d: hash=%016x anchorTime=%.1f\n", i, entries[i].Hash, entries[i].AnchorTime)
	}
}

func TestHashMatchClip(t *testing.T) {
	samples, err := loadMP3("../testdata/001.mp3")
	if err != nil {
		t.Fatalf("failed to load mp3: %v", err)
	}

	const clipLen = SAMPLE_RATE * 5
	const noiseLevel = 0.01

	if len(samples) < clipLen {
		t.Fatalf("001.mp3 is shorter than 5 seconds (%d samples)", len(samples))
	}

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	maxStart := len(samples) - clipLen
	startSample := rng.Intn(maxStart + 1)
	endSample := startSample + clipLen

	startSec := float64(startSample) / SAMPLE_RATE
	endSec := float64(endSample) / SAMPLE_RATE

	t.Logf("random seed  : %d", seed)
	t.Logf("clip window  : %.3fs – %.3fs", startSec, endSec)
	t.Logf("noise level  : %.0f%% of peak amplitude", noiseLevel*100)

	cleanClip := samples[startSample:endSample]
	cleanEntries := Process(context.Background(), cleanClip)

	if len(cleanEntries) == 0 {
		t.Fatal("clean clip produced no hash entries")
	}

	noisyClip := addNoise(noiseLevel, cleanClip, rng)
	noisyEntries := Process(context.Background(), noisyClip)

	cleanHS := hashSet(cleanEntries)

	var matched []uint64
	for _, e := range noisyEntries {
		if _, ok := cleanHS[e.Hash]; ok {
			matched = append(matched, e.Hash)
		}
	}

	pct := float64(len(matched)) / float64(len(cleanEntries)) * 100

	fmt.Printf("\nClean clip entries : %d\n", len(cleanEntries))
	fmt.Printf("Noisy clip entries : %d\n", len(noisyEntries))
	fmt.Printf("Matched            : %d / %d (%.2f%%)\n\n",
		len(matched), len(cleanEntries), pct)

	limit := min(200, len(matched))
	for i := 0; i < limit; i++ {
		fmt.Printf("match %d: %016x\n", i, matched[i])
	}
	if len(matched) > 200 {
		fmt.Printf("... and %d more\n", len(matched)-200)
	}
}