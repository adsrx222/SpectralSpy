package main

import (
	"context"
	"strconv"
	"syscall/js"

	"github.com/adsrx222/SpectralSpy/SpectralSpy"
)

func processSamples(this js.Value, args []js.Value) any {
	// ensure the frontend passed the sample array
	if len(args) < 1 {
		return js.ValueOf("Error: Missing float64 audio samples array")
	}

	jsSamples := args[0]
	length := jsSamples.Get("length").Int()
	samples := make([]float64, length)

	// translate js Float64Array into a native Go []float64 slice
	for i := 0; i < length; i++ {
		samples[i] = jsSamples.Index(i).Float()
	}

	fingerprints := SpectralSpy.Process(context.Background(), samples)

	// initialize a JavaScript Array to hold the returned fingerprints
	jsResult := js.Global().Get("Array").New(len(fingerprints))
	
	for i, fp := range fingerprints {
		jsObj := js.Global().Get("Object").New()
		
		// Convert the uint64 hash to a string
		jsObj.Set("hash", strconv.FormatUint(fp.Hash, 10))
		jsObj.Set("anchor_time", fp.AnchorTime)
		
		jsResult.SetIndex(i, jsObj)
	}

	return jsResult
}

func main() {
	c := make(chan struct{}, 0)
	js.Global().Set("processAudioSamples", js.FuncOf(processSamples))
	<-c
}