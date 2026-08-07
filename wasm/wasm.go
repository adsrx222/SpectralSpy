package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"syscall/js"

	"github.com/adsrx222/SpectralSpy/SpectralSpy"
)

// processAudioWasm receives the audio buffer from JavaScript
func processAudioWasm(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return `{"error": "No audio data provided by JavaScript"}`
	}

	jsData := args[0]
	length := jsData.Get("length").Int()

	// Create a Go byte slice to hold the raw memory buffer from JS
	byteData := make([]byte, length)
	
	// Copy the Uint8Array data from JS into Go memory securely
	js.CopyBytesToGo(byteData, jsData)

	// Reconstruct the raw bytes into float64s. 
	numSamples := length / 8
	samples := make([]float64, numSamples)

	for i := 0; i < numSamples; i++ {
		// JavaScript's Float64Array uses Little-Endian byte order. 
		bits := uint64(byteData[i*8]) |
			uint64(byteData[i*8+1])<<8 |
			uint64(byteData[i*8+2])<<16 |
			uint64(byteData[i*8+3])<<24 |
			uint64(byteData[i*8+4])<<32 |
			uint64(byteData[i*8+5])<<40 |
			uint64(byteData[i*8+6])<<48 |
			uint64(byteData[i*8+7])<<56
		
		samples[i] = math.Float64frombits(bits)
	}

	// Run the actual fingerprinting algorithm.
	// FIX: Use the blank identifier '_' to ignore the constellation points array
	hashes, _ := SpectralSpy.ProcessWithPeaks(context.Background(), samples)

	// Serialize to JSON and return it across the WASM boundary to JS
	jsonBytes, marshalErr := json.Marshal(hashes)
	if marshalErr != nil {
		return fmt.Sprintf(`{"error": "Failed to marshal JSON: %v"}`, marshalErr)
	}

	return string(jsonBytes)
}

func main() {
	// A channel to keep the Go WASM process alive indefinitely
	c := make(chan struct{}, 0)
	
	// Export the Go function to the global JavaScript 'window' object
	js.Global().Set("processAudioWasm", js.FuncOf(processAudioWasm))
	
	fmt.Println("Go WebAssembly Module Initialized.")
	<-c
}