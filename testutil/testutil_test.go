package testutil_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"spectralspy/pkg/SpectralSpy"
	"spectralspy/testutil"
)

func TestGenerateSilence(t *testing.T) {
	samples := testutil.GenerateSilence(2.0, SpectralSpy.SAMPLE_RATE)
	expectedLength := int(2.0 * float64(SpectralSpy.SAMPLE_RATE))
	if len(samples) != expectedLength {
		t.Errorf("expected silence length %d, got %d", expectedLength, len(samples))
	}
	for i, v := range samples {
		if v != 0.0 {
			t.Errorf("expected silent sample at index %d to be 0.0, got %f", i, v)
		}
	}
}

func TestGenerateRepetitiveKickDrum(t *testing.T) {
	samples := testutil.GenerateRepetitiveKickDrum(1.5, SpectralSpy.SAMPLE_RATE)
	expectedLength := int(1.5 * float64(SpectralSpy.SAMPLE_RATE))
	if len(samples) != expectedLength {
		t.Errorf("expected kick drum length %d, got %d", expectedLength, len(samples))
	}
	
	// ensure there are non-zero elements
	nonZeroCount := 0
	for _, v := range samples {
		if math.Abs(v) > 1e-4 {
			nonZeroCount++
		}
	}
	if nonZeroCount == 0 {
		t.Errorf("expected non-silent kick drum, but all samples were zero")
	}
}

func TestApplyEQCut(t *testing.T) {
	// generate a 1-second sine wave at 1000 Hz
	freq := 1000.0
	length := SpectralSpy.SAMPLE_RATE
	signal := make([]float64, length)
	for i := range signal {
		signal[i] = math.Sin(2.0 * math.Pi * freq * float64(i) / float64(SpectralSpy.SAMPLE_RATE))
	}

	// apply lowpass cut at 300Hz (should attenuate 1000Hz significantly)
	lowpassed := testutil.ApplyEQCut(signal, 300.0, "lowpass")
	if len(lowpassed) != length {
		t.Fatalf("expected lowpass output length %d, got %d", length, len(lowpassed))
	}

	// measure amplitude of the last 100 samples to ignore filter startup transient
	origAmp := 0.0
	lpAmp := 0.0
	for i := length - 100; i < length; i++ {
		origAmp += signal[i] * signal[i]
		lpAmp += lowpassed[i] * lowpassed[i]
	}
	
	if lpAmp >= origAmp {
		t.Errorf("expected lowpass filter at 300Hz to attenuate 1000Hz signal, but got lp RMS %.6f >= orig RMS %.6f", lpAmp, origAmp)
	}

	// apply highpass cut at 3000Hz
	highpassed := testutil.ApplyEQCut(signal, 3000.0, "highpass")
	if len(highpassed) != length {
		t.Fatalf("expected highpass output length %d, got %d", length, len(highpassed))
	}

	hpAmp := 0.0
	for i := length - 100; i < length; i++ {
		hpAmp += highpassed[i] * highpassed[i]
	}
	if hpAmp >= origAmp {
		t.Errorf("expected highpass filter at 3000Hz to attenuate 1000Hz signal, but got hp RMS %.6f >= orig RMS %.6f", hpAmp, origAmp)
	}
}

func TestApplyRoomReverb(t *testing.T) {
	length := 1000
	signal := make([]float64, length)
	signal[10] = 1.0 // Impulse

	// apply 3 echoes: 1ms, 2ms, 3ms (which are 44, 88, 132 samples at 44.1kHz)
	delayMs := []float64{1.0, 2.0, 3.0}
	decay := []float64{0.5, 0.25, 0.125}

	reverbed := testutil.ApplyRoomReverb(signal, delayMs, decay)
	if len(reverbed) != length {
		t.Fatalf("expected reverb output length %d, got %d", length, len(reverbed))
	}

	// sample 10 from original impulse to check echoes are present
	if reverbed[10] != 1.0 {
		t.Errorf("expected original impulse at index 10 to be 1.0, got %f", reverbed[10])
	}
	// echo 1: 44 samples later (at index 54) should be decay[0] * 1.0 = 0.5
	s1 := 10 + int(44)
	if math.Abs(reverbed[s1]-0.5) > 1e-6 {
		t.Errorf("expected echo at index %d to be 0.5, got %f", s1, reverbed[s1])
	}
	// echo 2: 88 samples later (at index 98) should be decay[1] * 1.0 = 0.25
	s2 := 10 + int(88)
	if math.Abs(reverbed[s2]-0.25) > 1e-6 {
		t.Errorf("expected echo at index %d to be 0.25, got %f", s2, reverbed[s2])
	}
}

func TestStratifiedSampleMaestro(t *testing.T) {
	workspaceDir := "../misc/workspace"
	// check if JSON exists before running integration test
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	if _, err := os.Stat(jsonPath); os.IsNotExist(err) {
		t.Skip("maestro JSON metadata file not found, skipping stratified sampling integration test")
	}

	sampled, err := testutil.StratifiedSampleMaestro(workspaceDir, 0.02)
	if err != nil {
		t.Fatalf("failed to run stratified sampling: %v", err)
	}

	if len(sampled) == 0 {
		t.Errorf("expected stratified sampling to return some tracks, got 0")
	}

	// verify noise tracks are appended
	foundSilence := false
	foundKickdrum := false
	for _, track := range sampled {
		if track.SongID == "noise_silence" && track.IsNoise && track.NoiseType == "silence" {
			foundSilence = true
		}
		if track.SongID == "noise_kickdrum" && track.IsNoise && track.NoiseType == "kick_drum" {
			foundKickdrum = true
		}
	}

	if !foundSilence {
		t.Errorf("expected micro-corpus to include classical silence noise track")
	}
	if !foundKickdrum {
		t.Errorf("expected micro-corpus to include electronic kickdrum noise track")
	}
}
