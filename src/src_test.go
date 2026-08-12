package SpectralSpy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adsrx222/SpectralSpy/src/fp-engine"
	"github.com/gin-gonic/gin"
)

func TestFindPeaks_SilenceNoPeaks(t *testing.T) {
	ctx := context.Background()
	sg := makeSpectrogram(makeSilence(SpectralSpyEngine.WINDOW_SIZE * 8))
	peaks := SpectralSpyEngine.FindPeaks(ctx, sg)
	for i, row := range peaks {
		if len(row) > 0 {
			t.Errorf("frame %d: expected 0 peaks for silence, got %d", i, len(row))
			return
		}
	}
}

func TestFindPeaks_SineHasPeaks(t *testing.T) {
	ctx := context.Background()
	sg := makeSpectrogram(makeSine(SpectralSpyEngine.WINDOW_SIZE*16, 1000))
	peaks := SpectralSpyEngine.FindPeaks(ctx, sg)
	total := 0
	for _, row := range peaks {
		total += len(row)
	}
	if total == 0 {
		t.Error("expected peaks for a 1 kHz sine wave, got none")
	}
}

func TestFindPeaks_PeaksAreSparse(t *testing.T) {
	ctx := context.Background()
	n := SpectralSpyEngine.WINDOW_SIZE * 32
	sg := makeSpectrogram(makeSine(n, 440))
	peaks := SpectralSpyEngine.FindPeaks(ctx, sg)

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
	ctx := context.Background()
	sg := makeSpectrogram(makeSine(SpectralSpyEngine.WINDOW_SIZE*16, 800))
	peaks := SpectralSpyEngine.FindPeaks(ctx, sg)

	numFrames := len(sg.Mags)
	numBins := len(sg.Mags[0])

	for t2, row := range peaks {
		for _, p := range row {
			bin := int(math.Round(p.Frequency / sg.BinHz))
			mag := sg.Mags[t2][bin]

			for dt := -SpectralSpyEngine.PEAK_NEIGHBORHOOD_TIME; dt <= SpectralSpyEngine.PEAK_NEIGHBORHOOD_TIME; dt++ {
				nt := t2 + dt
				if nt < 0 || nt >= numFrames {
					continue
				}
				for df := -SpectralSpyEngine.PEAK_NEIGHBORHOOD_FREQ; df <= SpectralSpyEngine.PEAK_NEIGHBORHOOD_FREQ; df++ {
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
	ctx := context.Background()
	sg := makeSpectrogram(makeSine(SpectralSpyEngine.WINDOW_SIZE*8, 440))
	for _, row := range SpectralSpyEngine.FindPeaks(ctx, sg) {
		for _, p := range row {
			if p.Magnitude < 0 {
				t.Errorf("negative magnitude in peak: %f", p.Magnitude)
			}
		}
	}
}

func TestFindPeaks_BinIndexConsistent(t *testing.T) {
	ctx := context.Background()
	sg := makeSpectrogram(makeSine(SpectralSpyEngine.WINDOW_SIZE*8, 1000))
	for _, row := range SpectralSpyEngine.FindPeaks(ctx, sg) {
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

func makePoint(ts, freq, mag float64) SpectralSpyEngine.ConstellationPoint {
	return SpectralSpyEngine.ConstellationPoint{
		Timestamp: ts,
		Frequency: freq,
		Magnitude: mag,
		BinIndex:  int(math.Round(freq / (float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)))),
	}
}

func TestHashPair_DifferentTethersDifferentHash(t *testing.T) {
	a := makePoint(1.0, 440.0, 100.0)
	b := makePoint(2.0, 880.0, 200.0)
	c := makePoint(3.0, 1760.0, 50.0)
	if SpectralSpyEngine.HashPair(a, b) == SpectralSpyEngine.HashPair(a, c) {
		t.Error("different tether points produced the same hash")
	}
}

func TestHashPair_NonZero(t *testing.T) {
	a := makePoint(1.0, 440.0, 1.0)
	b := makePoint(2.0, 880.0, 1.0)
	if SpectralSpyEngine.HashPair(a, b) == 0 {
		t.Error("hash should not be zero for non-trivial inputs")
	}
}

func TestHashPair_FrequencyMatters(t *testing.T) {
	base := makePoint(0.0, 440.0, 1.0)
	same := makePoint(0.0, 440.0, 1.0)
	diff := makePoint(0.0, 880.0, 1.0)
	if SpectralSpyEngine.HashPair(base, same) == SpectralSpyEngine.HashPair(base, diff) {
		t.Error("different tether frequencies must produce different hashes")
	}
}

func TestHashPair_DeltaTimeMatters(t *testing.T) {
	anchor := makePoint(0.0, 440.0, 1.0)
	t1 := makePoint(1.0, 880.0, 1.0)
	t2 := makePoint(2.0, 880.0, 1.0)
	if SpectralSpyEngine.HashPair(anchor, t1) == SpectralSpyEngine.HashPair(anchor, t2) {
		t.Error("different time deltas must produce different hashes")
	}
}

func TestHashPair_PositionIndependent(t *testing.T) {
	anchor1 := makePoint(10.0, 440.0, 1.0)
	tether1 := makePoint(12.0, 880.0, 1.0)

	anchor2 := makePoint(50.0, 440.0, 1.0)
	tether2 := makePoint(52.0, 880.0, 1.0)

	if SpectralSpyEngine.HashPair(anchor1, tether1) != SpectralSpyEngine.HashPair(anchor2, tether2) {
		t.Error("pairs with same (f_A, f_B, ΔT) at different positions must hash identically")
	}
}

func TestHashPair_AbsolutePositionDoesNotMatter(t *testing.T) {
	const shift = 1000.0
	anchor := makePoint(5.0, 440.0, 1.0)
	tether := makePoint(7.0, 880.0, 1.0)

	shiftedAnchor := makePoint(5.0+shift, 440.0, 1.0)
	shiftedTether := makePoint(7.0+shift, 880.0, 1.0)

	if SpectralSpyEngine.HashPair(anchor, tether) != SpectralSpyEngine.HashPair(shiftedAnchor, shiftedTether) {
		t.Error("shifting both anchor and tether by a constant must not change hash")
	}
}

// generateHashEntries()
func makePeaks(numFrames, peaksPerFrame int, baseFreq, baseTime float64) [][]SpectralSpyEngine.ConstellationPoint {
	binHz := float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)
	pm := make([][]SpectralSpyEngine.ConstellationPoint, numFrames)
	for r := range pm {
		pm[r] = make([]SpectralSpyEngine.ConstellationPoint, peaksPerFrame)
		for c := range pm[r] {
			freq := baseFreq + float64(c)*binHz*float64(SpectralSpyEngine.TARGET_ZONE_FREQ_BINS)
			bin := int(math.Round(freq / binHz))
			pm[r][c] = SpectralSpyEngine.ConstellationPoint {
				Timestamp: baseTime + float64(r),
				Frequency: freq,
				Magnitude: 1.0,
				BinIndex:  bin,
			}
		}
	}
	return pm
}

func TestGenerateHashEntries_NonEmpty(t *testing.T) {
	ctx := context.Background()
	peaks := makePeaks(SpectralSpyEngine.TARGET_ZONE_TIME_END+2, 3, 440.0, 0.0)
	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)
	if len(entries) == 0 {
		t.Error("expected hash entries for populated peaks, got none")
	}
}

func TestGenerateHashEntries_Deterministic(t *testing.T) {
	ctx := context.Background()

	peaks := makePeaks(20, 4, 440.0, 0.0)
	e1 := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)
	e2 := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)
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
	ctx := context.Background()
	peaks := makePeaks(SpectralSpyEngine.TARGET_ZONE_TIME_END+3, 2, 440.0, 100.0)
	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)
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
	ctx := context.Background()
	peaks := makePeaks(SpectralSpyEngine.TARGET_ZONE_TIME_END+5, 2, 440.0, 0.0)
	maxAnchorTime := 0.0
	for _, row := range peaks {
		for _, p := range row {
			if p.Timestamp > maxAnchorTime {
				maxAnchorTime = p.Timestamp
			}
		}
	}
	for i, e := range SpectralSpyEngine.GenerateHashEntries(ctx, peaks) {
		if e.AnchorTime > maxAnchorTime {
			t.Errorf("entry %d: AnchorTime %f > max peak time %f", i, e.AnchorTime, maxAnchorTime)
		}
	}
}

func TestGenerateHashEntries_NoBoundaryDuplication(t *testing.T) {
	ctx := context.Background()
	peaks := makePeaks(40, 3, 440.0, 0.0)
	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)

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
	ctx := context.Background()
	p1 := makePeaks(SpectralSpyEngine.TARGET_ZONE_TIME_END+2, 2, 440.0, 0.0)
	p2 := makePeaks(SpectralSpyEngine.TARGET_ZONE_TIME_END+2, 2, 880.0, 0.0)
	h1 := hashesFrom(SpectralSpyEngine.GenerateHashEntries(ctx, p1))
	h2 := hashesFrom(SpectralSpyEngine.GenerateHashEntries(ctx, p2))

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

// SpectralSpyEngine.Process()
func TestProcess_ReturnsSomething(t *testing.T) {
	entries := SpectralSpyEngine.Process(context.Background(), makeSine(SpectralSpyEngine.SAMPLE_RATE*2, 440))
	if len(entries) == 0 {
		t.Error("Process returned no entries for a 2-second sine wave")
	}
}

func TestProcess_Deterministic(t *testing.T) {
	samples := makeSine(SpectralSpyEngine.SAMPLE_RATE, 1000)
	e1 := SpectralSpyEngine.Process(context.Background(), samples)
	e2 := SpectralSpyEngine.Process(context.Background(), samples)
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
	n := SpectralSpyEngine.SAMPLE_RATE * 2
	eSilence := SpectralSpyEngine.Process(context.Background(), makeSilence(n))
	eSine := SpectralSpyEngine.Process(context.Background(), makeSine(n, 440))

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
		SpectralSpyEngine.Process(ctx, makeSilence(SpectralSpyEngine.SAMPLE_RATE*60))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Process did not return after context cancellation")
	}
}

func TestProcess_EmptyInput(t *testing.T) {
	entries := SpectralSpyEngine.Process(context.Background(), []float64{})
	if entries == nil {
		return
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty input, got %d", len(entries))
	}
}

func TestProcess_AnchorTimesNonNegative(t *testing.T) {
	for _, e := range SpectralSpyEngine.Process(context.Background(), makeSine(SpectralSpyEngine.SAMPLE_RATE, 440)) {
		if e.AnchorTime < 0 {
			t.Errorf("negative AnchorTime: %f", e.AnchorTime)
			return
		}
	}
}

func TestProcess_PositionIndependentHashes(t *testing.T) {
	sine := makeSine(SpectralSpyEngine.WINDOW_SIZE*32, 1000)

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	noisySine := addNoise(sine, 0.01, rng)

	e1 := SpectralSpyEngine.Process(context.Background(), noisySine)
	padded := append(makeSilence(SpectralSpyEngine.WINDOW_SIZE*8), noisySine...)
	e2 := SpectralSpyEngine.Process(context.Background(), padded)

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
	samples, err := findRandomSlice(WAVEFORM_PATH, 8)
	if err != nil {
		t.Fatalf("failed to load random wav: %v", err)
	}

	entries := SpectralSpyEngine.Process(context.Background(), samples)
	if len(entries) == 0 {
		t.Error("Process returned no entries for random WAV")
	}

	// fmt.Printf("\nGenerated %d hash entries\n", len(entries))
	// limit := min(200, len(entries))
	// for i := 0; i < limit; i++ {
	// 	fmt.Printf("%d: hash=%016x anchorTime=%.1f\n", i, entries[i].Hash, entries[i].AnchorTime)
	// }
}

// Test Matching

func TestHashMatchClip(t *testing.T) {
	samples, err := findRandomSlice(WAVEFORM_PATH, 8)
	if err != nil {
		t.Fatalf("failed to load WAV: %v", err)
	}

	const clipLen = SpectralSpyEngine.SAMPLE_RATE * 5
	const noiseLevel = 0.01

	if len(samples) < clipLen {
		t.Fatalf("RANDOM WAV is shorter than 5 seconds (%d samples)", len(samples))
	}

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	maxStart := len(samples) - clipLen
	startSample := rng.Intn(maxStart + 1)
	endSample := startSample + clipLen

	startSec := float64(startSample) / SpectralSpyEngine.SAMPLE_RATE
	endSec := float64(endSample) / SpectralSpyEngine.SAMPLE_RATE

	t.Logf("random seed  : %d", seed)
	t.Logf("clip window  : %.3fs – %.3fs", startSec, endSec)
	t.Logf("noise level  : %.0f%% of peak amplitude", noiseLevel*100)

	cleanClip := samples[startSample:endSample]
	cleanEntries := SpectralSpyEngine.Process(context.Background(), cleanClip)

	if len(cleanEntries) == 0 {
		t.Fatal("clean clip produced no hash entries")
	}

	noisyClip := addNoise(cleanClip, noiseLevel, rng)
	noisyEntries := SpectralSpyEngine.Process(context.Background(), noisyClip)

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
func TestFanOut_SingleAnchorCapped(t *testing.T) {
	ctx := context.Background()
	binHz := float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)

	numFrames := SpectralSpyEngine.TARGET_ZONE_TIME_END + 3
	peaks := make([][]SpectralSpyEngine.ConstellationPoint, numFrames)

	// single anchor at frame 0
	anchorFreq := 2000.0
	anchorBin := int(math.Round(anchorFreq / binHz))
	peaks[0] = []SpectralSpyEngine.ConstellationPoint{{
		Timestamp: 0,
		Frequency: float64(anchorBin) * binHz,
		Magnitude: 1.0,
		BinIndex:  anchorBin,
	}}

	for f := SpectralSpyEngine.TARGET_ZONE_TIME_START; f <= SpectralSpyEngine.TARGET_ZONE_TIME_END && f < numFrames; f++ {
		var framePeaks []SpectralSpyEngine.ConstellationPoint
		for b := anchorBin - SpectralSpyEngine.TARGET_ZONE_FREQ_BINS; b <= anchorBin+SpectralSpyEngine.TARGET_ZONE_FREQ_BINS; b++ {
			if b < 0 {
				continue
			}
			framePeaks = append(framePeaks, SpectralSpyEngine.ConstellationPoint{
				Timestamp: float64(f),
				Frequency: float64(b) * binHz,
				Magnitude: 1.0,
				BinIndex:  b,
			})
		}
		peaks[f] = framePeaks
	}

	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)

	count := 0
	for _, e := range entries {
		if e.AnchorTime == 0 {
			count++
		}
	}

	potential := (SpectralSpyEngine.TARGET_ZONE_TIME_END - SpectralSpyEngine.TARGET_ZONE_TIME_START + 1) * (2*SpectralSpyEngine.TARGET_ZONE_FREQ_BINS + 1)

	if count > SpectralSpyEngine.MAX_FAN_OUT {
		t.Errorf("single anchor produced %d entries, exceeding SpectralSpyEngine.MAX_FAN_OUT=%d (potential=%d)",
			count, SpectralSpyEngine.MAX_FAN_OUT, potential)
	}
	if count != SpectralSpyEngine.MAX_FAN_OUT {
		t.Errorf("expected exactly SpectralSpyEngine.MAX_FAN_OUT=%d entries when %d tethers available, got %d",
			SpectralSpyEngine.MAX_FAN_OUT, potential, count)
	}
}

func TestFanOut_DensePeaks_RespectsCap(t *testing.T) {
	ctx := context.Background()
	binHz := float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)

	numFrames := 20
	peaksPerFrame := 5
	peaks := make([][]SpectralSpyEngine.ConstellationPoint, numFrames)

	for f := 0; f < numFrames; f++ {
		peaks[f] = make([]SpectralSpyEngine.ConstellationPoint, peaksPerFrame)
		for p := 0; p < peaksPerFrame; p++ {
			bin := 50 + p
			peaks[f][p] = SpectralSpyEngine.ConstellationPoint{
				Timestamp: float64(f),
				Frequency: float64(bin) * binHz,
				Magnitude: 1.0,
				BinIndex:  bin,
			}
		}
	}

	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)

	perFrame := make(map[float64]int)
	for _, e := range entries {
		perFrame[e.AnchorTime]++
	}

	for ts, count := range perFrame {
		maxAllowed := peaksPerFrame * SpectralSpyEngine.MAX_FAN_OUT
		if count > maxAllowed {
			t.Errorf("frame t=%.0f: %d entries exceeds max %d (%d anchors × %d fan-out)",
				ts, count, maxAllowed, peaksPerFrame, SpectralSpyEngine.MAX_FAN_OUT)
		}
	}
}

func TestFanOut_SparseBelowCap(t *testing.T) {
	ctx := context.Background()
	binHz := float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)

	numFrames := SpectralSpyEngine.TARGET_ZONE_TIME_END + 3
	peaks := make([][]SpectralSpyEngine.ConstellationPoint, numFrames)

	anchorBin := 100
	peaks[0] = []SpectralSpyEngine.ConstellationPoint{{
		Timestamp: 0,
		Frequency: float64(anchorBin) * binHz,
		Magnitude: 1.0,
		BinIndex:  anchorBin,
	}}

	tethersPlaced := 0
	for f := SpectralSpyEngine.TARGET_ZONE_TIME_START; f <= SpectralSpyEngine.TARGET_ZONE_TIME_END && f < numFrames && tethersPlaced < 3; f++ {
		peaks[f] = []SpectralSpyEngine.ConstellationPoint{{
			Timestamp: float64(f),
			Frequency: float64(anchorBin) * binHz,
			Magnitude: 1.0,
			BinIndex:  anchorBin,
		}}
		tethersPlaced++
	}

	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)

	count := 0
	for _, e := range entries {
		if e.AnchorTime == 0 {
			count++
		}
	}

	if count != 3 {
		t.Errorf("expected 3 entries for %d reachable tethers (below cap=%d), got %d",
			3, SpectralSpyEngine.MAX_FAN_OUT, count)
	}
}

func TestFanOut_ExactlyAtCap(t *testing.T) {
	ctx := context.Background()
	binHz := float64(SpectralSpyEngine.SAMPLE_RATE) / float64(SpectralSpyEngine.WINDOW_SIZE)

	numFrames := SpectralSpyEngine.TARGET_ZONE_TIME_END + 3
	peaks := make([][]SpectralSpyEngine.ConstellationPoint, numFrames)

	anchorBin := 150
	peaks[0] = []SpectralSpyEngine.ConstellationPoint{{
		Timestamp: 0,
		Frequency: float64(anchorBin) * binHz,
		Magnitude: 1.0,
		BinIndex:  anchorBin,
	}}

	placed := 0
	for f := SpectralSpyEngine.TARGET_ZONE_TIME_START; f <= SpectralSpyEngine.TARGET_ZONE_TIME_END && f < numFrames && placed < SpectralSpyEngine.MAX_FAN_OUT; f++ {
		var framePeaks []SpectralSpyEngine.ConstellationPoint
		for p := 0; p < 2 && placed < SpectralSpyEngine.MAX_FAN_OUT; p++ {
			b := anchorBin + p
			framePeaks = append(framePeaks, SpectralSpyEngine.ConstellationPoint{
				Timestamp: float64(f),
				Frequency: float64(b) * binHz,
				Magnitude: 1.0,
				BinIndex:  b,
			})
			placed++
		}
		peaks[f] = framePeaks
	}

	entries := SpectralSpyEngine.GenerateHashEntries(ctx, peaks)

	count := 0
	for _, e := range entries {
		if e.AnchorTime == 0 {
			count++
		}
	}

	if count != SpectralSpyEngine.MAX_FAN_OUT {
		t.Errorf("expected exactly SpectralSpyEngine.MAX_FAN_OUT=%d entries when exactly %d tethers available, got %d",
			SpectralSpyEngine.MAX_FAN_OUT, SpectralSpyEngine.MAX_FAN_OUT, count)
	}
}

func TestProcess_MemoryLeak(t *testing.T) {
	// generate 2s of silent audio samples
	samples := make([]float64, SpectralSpyEngine.SAMPLE_RATE*2)

	var m1, m2 runtime.MemStats

	for i := 0; i < 5; i++ {
		_ = SpectralSpyEngine.Process(context.Background(), samples)
	}

	runtime.GC()
	runtime.ReadMemStats(&m1)

	for i := 0; i < 500; i++ {
		_ = SpectralSpyEngine.Process(context.Background(), samples)
	}

	runtime.GC() // Force gc to see retained heap
	runtime.ReadMemStats(&m2)

	// use signed arithmetic to handle GC fluctuations that can reduce alloc below m1
	growth := int64(m2.HeapInuse) - int64(m1.HeapInuse)

	// allow for baseline test overhead (e.g., 5MB limit)
	if growth > 5*1024*1024 {
		t.Errorf("Potential memory leak: HeapInuse grew by %d bytes after 50 iterations", growth)
	}
}

// Context Cancellation & Timeout Testing
func TestProcess_ContextCancellation(t *testing.T) {
	samples := make([]float64, SpectralSpyEngine.SAMPLE_RATE*2)

	// Create an already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_ = SpectralSpyEngine.Process(ctx, samples)
		close(done)
	}()

	select {
	case <-done:
		// Success - goroutine returned promptly
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Process did not observe context cancellation promptly")
	}
}

func TestProcess_ContextTimeout(t *testing.T) {
	samples := make([]float64, SpectralSpyEngine.SAMPLE_RATE*10) // 10 seconds of data

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = SpectralSpyEngine.Process(ctx, samples)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not terminate on timeout expiration")
	}
}

// Data Race Testing
func TestDataRace_ConcurrentProcess(t *testing.T) {
	samples := makeSine(SpectralSpyEngine.SAMPLE_RATE*2, 440)
	const workers = 8

	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_ = SpectralSpyEngine.Process(context.Background(), samples)
			done <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}

func TestDataRace_ConcurrentProcessWithPeaks(t *testing.T) {
	samples := makeSine(SpectralSpyEngine.SAMPLE_RATE, 1000)
	const workers = 6

	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, _ = SpectralSpyEngine.ProcessWithPeaks(context.Background(), samples)
			done <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}

func TestDataRace_ConcurrentMatchFingerprints(t *testing.T) {
	fps := SpectralSpyEngine.Process(context.Background(), makeSine(SpectralSpyEngine.SAMPLE_RATE, 440))
	db := map[uint64][]SpectralSpyEngine.DBEntry{}
	for _, fp := range fps {
		db[fp.Hash] = append(db[fp.Hash], SpectralSpyEngine.DBEntry{
			Hash: fp.Hash, SongID: "s1", AnchorTime: fp.AnchorTime, Weight: 1.0,
		})
	}

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, _, _, _ = SpectralSpyEngine.MatchFingerprints(fps, db)
			done <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}

func TestProcess_NoCancelledContextGoroutineLeak(t *testing.T) {
	samples := makeSilence(SpectralSpyEngine.SAMPLE_RATE * 2)

	// run several cancelled-context calls; verify goroutines shutoff
	baseline := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = SpectralSpyEngine.Process(ctx, samples)
	}

	// give any stray goroutines time to exit.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()

	after := runtime.NumGoroutine()
	// allow a small delta for test framework overhead.
	if after > baseline+3 {
		t.Errorf("goroutine leak after cancelled contexts: baseline=%d, after=%d", baseline, after)
	}
}

// MatchFingerprints Testing Suite

func TestMatchFingerprints_OffsetMarginConfidence(t *testing.T) {
	db := map[uint64][]SpectralSpyEngine.DBEntry{
		10: {{Hash: 10, SongID: "A", AnchorTime: 0.0, Weight: 1.0}},
		11: {{Hash: 11, SongID: "A", AnchorTime: SpectralSpyEngine.HOP_SIZE, Weight: 1.0}},
		12: {{Hash: 12, SongID: "A", AnchorTime: 2 * SpectralSpyEngine.HOP_SIZE, Weight: 1.0}},
		20: {{Hash: 20, SongID: "B", AnchorTime: 0.0, Weight: 1.0}},
	}

	query := []SpectralSpyEngine.Fingerprint{
		{Hash: 10, AnchorTime: 0.0},
		{Hash: 11, AnchorTime: SpectralSpyEngine.HOP_SIZE},
		{Hash: 12, AnchorTime: 2 * SpectralSpyEngine.HOP_SIZE},
		{Hash: 20, AnchorTime: 0.0},
	}

	song, _, _, confidence := SpectralSpyEngine.MatchFingerprints(query, db)
	if song != "A" {
		t.Errorf("expected song A, got %s", song)
	}
	if confidence <= 1.0 {
		t.Errorf("expected confidence > 1 when A dominates, got %.3f", confidence)
	}
}

// TestMatchFingerprints_InsufficientData verifies that a single fingerprint
// still returns a valid (non-crashing) result.
func TestMatchFingerprints_InsufficientData(t *testing.T) {
	db := map[uint64][]SpectralSpyEngine.DBEntry{
		42: {{Hash: 42, SongID: "X", AnchorTime: 5.0, Weight: 1.0}},
	}

	query := []SpectralSpyEngine.Fingerprint{{Hash: 42, AnchorTime: 5.0}}
	song, score, _, _ := SpectralSpyEngine.MatchFingerprints(query, db)
	if song != "X" {
		t.Errorf("expected X, got %q", song)
	}
	if score == 0 {
		t.Error("expected non-zero score for single matching fingerprint")
	}
}

// TestMatchFingerprints_DuplicateCollidingHashes verifies that duplicate query
// fingerprints (hash collision within the query) are counted independently.
func TestMatchFingerprints_DuplicateCollidingHashes(t *testing.T) {
	db := map[uint64][]SpectralSpyEngine.DBEntry{
		77: {{Hash: 77, SongID: "dup", AnchorTime: 1.0, Weight: 1.0}},
	}

	// Duplicate hash in query — should accumulate votes for the same bin.
	query := []SpectralSpyEngine.Fingerprint{
		{Hash: 77, AnchorTime: 1.0},
		{Hash: 77, AnchorTime: 1.0},
		{Hash: 77, AnchorTime: 1.0},
	}
	song, score, _, _ := SpectralSpyEngine.MatchFingerprints(query, db)
	if song != "dup" {
		t.Errorf("expected dup, got %q", song)
	}
	if score < 2.0 {
		t.Errorf("expected score >= 2 for 3 duplicate query hashes, got %.1f", score)
	}
}

// TestMatchFingerprints_BoundaryEmptyDB verifies correct behavior with an
// empty database map.
func TestMatchFingerprints_BoundaryEmptyDB(t *testing.T) {
	query := []SpectralSpyEngine.Fingerprint{{Hash: 999, AnchorTime: 1.0}}
	song, score, offset, confidence := SpectralSpyEngine.MatchFingerprints(query, map[uint64][]SpectralSpyEngine.DBEntry{})
	if song != "" || score != 0 || offset != 0 || confidence != 0 {
		t.Errorf("expected all-zero results for empty DB, got song=%q score=%.1f offset=%.1f confidence=%.1f",
			song, score, offset, confidence)
	}
}

// TestMatchFingerprints_BoundaryNilDB verifies correct behavior with a nil DB.
func TestMatchFingerprints_BoundaryNilDB(t *testing.T) {
	query := []SpectralSpyEngine.Fingerprint{{Hash: 1, AnchorTime: 0.0}}
	song, score, _, _ := SpectralSpyEngine.MatchFingerprints(query, nil)
	if song != "" || score != 0 {
		t.Errorf("expected empty result for nil DB, got song=%q score=%.1f", song, score)
	}
}

// TestMatchFingerprints_CorrectSongIdentification uses the real SpectralSpyEngine.Process
// pipeline (sine wave) to verify the matched song is consistent.
func TestMatchFingerprints_CorrectSongIdentification(t *testing.T) {
	samples := makeSine(SpectralSpyEngine.SAMPLE_RATE*2, 880)
	fps := SpectralSpyEngine.Process(context.Background(), samples)
	if len(fps) == 0 {
		t.Fatal("no fingerprints generated")
	}

	db := map[uint64][]SpectralSpyEngine.DBEntry{}
	for _, fp := range fps {
		db[fp.Hash] = append(db[fp.Hash], SpectralSpyEngine.DBEntry{
			Hash: fp.Hash, SongID: "sine880", AnchorTime: fp.AnchorTime, Weight: 1.0,
		})
	}

	// Query with the same fingerprints; must match the registered song.
	song, score, _, _ := SpectralSpyEngine.MatchFingerprints(fps, db)
	if song != "sine880" {
		t.Errorf("expected sine880, got %q", song)
	}
	if score == 0 {
		t.Error("expected non-zero score for exact match")
	}
}

func TestMatchFingerprints(t *testing.T) {
	db := map[uint64][]SpectralSpyEngine.DBEntry{
		100: {{Hash: 100, SongID: "song-A", AnchorTime: 1.0, Weight: 1.0}},
		200: {{Hash: 200, SongID: "song-A", AnchorTime: 1.5, Weight: 1.0}},
		300: {{Hash: 300, SongID: "song-B", AnchorTime: 0.5, Weight: 1.0}},
	}

	tests := []struct {
		name          string
		query         []SpectralSpyEngine.Fingerprint
		expectedSong  string
		expectedScore float64
	}{
		{
			name: "Exact Match",
			query: []SpectralSpyEngine.Fingerprint{
				{Hash: 100, AnchorTime: 1.0},
				{Hash: 200, AnchorTime: 1.5},
			},
			expectedSong:  "song-A",
			expectedScore: 2.0,
		},
		{
			name: "Offset Match",
			query: []SpectralSpyEngine.Fingerprint{
				{Hash: 100, AnchorTime: 2.0}, // 1 second late
				{Hash: 200, AnchorTime: 2.5}, // 1 second late
			},
			expectedSong:  "song-A",
			expectedScore: 2.0,
		},
		{
			name: "Partial Match Multi-Song",
			query: []SpectralSpyEngine.Fingerprint{
				{Hash: 300, AnchorTime: 1.0},
				{Hash: 200, AnchorTime: 2.0},
			},
			// Tied at 1, map iteration order determines winner, but both exist
			expectedScore: 1.0,
		},
		{
			name: "No Match",
			query: []SpectralSpyEngine.Fingerprint{
				{Hash: 999, AnchorTime: 1.0},
			},
			expectedSong:  "",
			expectedScore: 0.0,
		},
		{
			name:          "Empty Query",
			query:         []SpectralSpyEngine.Fingerprint{},
			expectedSong:  "",
			expectedScore: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			song, score, _, _ := SpectralSpyEngine.MatchFingerprints(tt.query, db)

			if tt.expectedScore > 0 && score != tt.expectedScore {
				t.Errorf("Expected score %f, got %f", tt.expectedScore, score)
			}
			if tt.expectedSong != "" && song != tt.expectedSong {
				t.Errorf("Expected song %s, got %s", tt.expectedSong, song)
			}
		})
	}
}

func setupTestRouter(t *testing.T) (*gin.Engine, *sql.DB) {
	t.Helper()

	// SetupTestDB is assumed to be in the same package (formerly testutil.go)
	dbConn := setupTestDB(t)

	seeds := []string{
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (101, 'songA', 4096.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (102, 'songA', 4196.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (103, 'songB', 8192.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (-1, 'songLargeHash', 2048.0)",
	}

	for _, seed := range seeds {
		if _, err := dbConn.Exec(seed); err != nil {
			t.Fatalf("Failed to seed database with query [%s]: %v", seed, err)
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	r := gin.New()
	r.POST("/api/v1/identify", NewIdentifyHandler(dbConn, logger))

	return r, dbConn
}

func TestHandleIdentify_Success(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	reqPayload := IdentifyRequest{
		Fingerprints: []SpectralSpyEngine.Fingerprint{
			{Hash: 101, AnchorTime: 0.0},
			{Hash: 102, AnchorTime: 100.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp IdentifyResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response JSON: %v", err)
	}

	if resp.SongID != "songA" {
		t.Errorf("Expected SongID 'songA', got '%s'", resp.SongID)
	}

	// Check if the offset is within a reasonable tolerance
	expectedOffset := 0.092880
	tolerance := 0.001
	if math.Abs(resp.TimeOffset-expectedOffset) > tolerance {
		t.Errorf("Expected TimeOffset within %f of %f, got %f", tolerance, expectedOffset, resp.TimeOffset)
	}
}

func TestHandleIdentify_ValidationErrors(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	tests := []struct {
		name         string
		payload      IdentifyRequest
		expectedCode int
	}{
		{
			name:         "Empty Fingerprints",
			payload:      IdentifyRequest{Fingerprints: []SpectralSpyEngine.Fingerprint{}},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Exceeds Fingerprint Limit",
			payload:      IdentifyRequest{Fingerprints: make([]SpectralSpyEngine.Fingerprint, 500001)},
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "No Match In DB",
			payload: IdentifyRequest{
				Fingerprints: []SpectralSpyEngine.Fingerprint{{Hash: 99999, AnchorTime: 0}},
			},
			expectedCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.payload)
			req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			r.ServeHTTP(rr, req)

			if rr.Code != tc.expectedCode {
				t.Errorf("Expected code %d, got %d", tc.expectedCode, rr.Code)
			}
		})
	}
}

func TestHandleIdentify_MalformedJSON(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	badJSON := []byte(`{"fingerprints": [{"hash": 101, "anchor_time": }]}`)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(badJSON))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 Bad Request for malformed JSON, got %d", rr.Code)
	}
}

func TestHandleIdentify_ExceedsMaxBytes(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	largePadding := strings.Repeat("a", 110*1024)
	payload := map[string]string{"padding": largePadding}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 Bad Request when exceeding max body bytes, got %d", rr.Code)
	}
}

func TestHandleIdentify_LargeUint64Hash(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	maxHash := uint64(18446744073709551615)
	reqPayload := IdentifyRequest{
		Fingerprints: []SpectralSpyEngine.Fingerprint{
			{Hash: maxHash, AnchorTime: 0.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200 for large uint64 hash, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp IdentifyResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.SongID != "songLargeHash" {
		t.Errorf("Expected SongID 'songLargeHash', got '%s'", resp.SongID)
	}
}

func TestHandleIdentify_DuplicateHashes(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	reqPayload := IdentifyRequest{
		Fingerprints: []SpectralSpyEngine.Fingerprint{
			{Hash: 101, AnchorTime: 0.0},
			{Hash: 101, AnchorTime: 2048.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200 for duplicate hash query, got %d", rr.Code)
	}
}