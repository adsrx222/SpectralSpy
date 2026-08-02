package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"spectralspy/ml"
	"spectralspy/testutil"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	// prevent system sleep on macOS during execution
	caffeinate := exec.Command("caffeinate", "-dimsu")
	if err := caffeinate.Start(); err == nil {
		defer func() {
			if caffeinate.Process != nil {
				_ = caffeinate.Process.Kill()
			}
		}()
	}

	workspaceDir := flag.String("workspace", "misc/workspace", "Path to workspace directory")
	w1 := flag.Float64("w1", 100.0, "Weight for Accuracy (0.0 to 1.0 ratio)")
	w2 := flag.Float64("w2", 1.0, "Weight for log(HashDensity)")
	w3 := flag.Float64("w3", 0.1, "Weight for Average Query Time (ms)")
	pct := flag.Float64("pct", 0.02, "Stratified sampling percentage (default 0.02)")
	queryCount := flag.Int("queries", 50, "Number of evaluation queries")
	maxIterations := flag.Int("iterations", 25, "Number of optimization iterations")
	flag.Parse()

	fmt.Println("====================================================")
	fmt.Println("   LOCAL NEIGHBORHOOD FITNESS SWEEP OPTIMIZER")
	fmt.Println("====================================================")
	fmt.Printf("Fixed Parameters : NFrames=5, NFreqs=5\n")
	fmt.Printf("Sweep Ranges     : TimeStart ∈ [0, 100], TimeEnd ∈ [1, 100], FreqBins ∈ [0, 20]\n")
	fmt.Printf("Configured Weights: w1=%.2f, w2=%.2f, w3=%.2f\n", *w1, *w2, *w3)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()

	corpus, err := testutil.StratifiedSampleMaestro(*workspaceDir, *pct)
	if err != nil {
		fmt.Printf("Error generating stratified micro-corpus: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated micro-corpus with %d tracks.\n", len(corpus))

	queries, err := testutil.GenerateEvaluationQuerySet(*workspaceDir, corpus, *queryCount)
	if err != nil {
		fmt.Printf("Error generating evaluation query set: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated %d evaluation queries.\n", len(queries))

	constFixedNFrames := 5
	constFixedNFreqs := 5

	var allPoints []ml.Params
	for ts := 0; ts <= 100; ts++ {
		for te := ts + 1; te <= 100; te++ {
			for fb := 0; fb <= 20; fb++ {
				allPoints = append(allPoints, ml.Params{
					NFrames:   constFixedNFrames,
					NFreqs:    constFixedNFreqs,
					TimeStart: ts,
					TimeEnd:   te,
					FreqBins:  fb,
				})
			}
		}
	}

	initialPoints := []ml.Params{
		{NFrames: 5, NFreqs: 5, TimeStart: 2, TimeEnd: 5, FreqBins: 8},
		{NFrames: 5, NFreqs: 5, TimeStart: 10, TimeEnd: 20, FreqBins: 10},
		{NFrames: 5, NFreqs: 5, TimeStart: 0, TimeEnd: 10, FreqBins: 5},
		{NFrames: 5, NFreqs: 5, TimeStart: 50, TimeEnd: 60, FreqBins: 15},
		{NFrames: 5, NFreqs: 5, TimeStart: 25, TimeEnd: 30, FreqBins: 20},
		{NFrames: 5, NFreqs: 5, TimeStart: 5, TimeEnd: 50, FreqBins: 2},
		{NFrames: 5, NFreqs: 5, TimeStart: 80, TimeEnd: 95, FreqBins: 12},
	}

	evaluated := make(map[ml.Params]ml.Trial)
	var trials []ml.Trial

	evaluatePoint := func(p ml.Params) (ml.Trial, error) {
		if t, ok := evaluated[p]; ok {
			return t, nil
		}
		fmt.Printf("Evaluating candidate: TimeStart=%d, TimeEnd=%d, FreqBins=%d ... ", p.TimeStart, p.TimeEnd, p.FreqBins)
		trial, err := ml.EvaluateParams(ctx, *workspaceDir, corpus, queries, p, *w1, *w2, *w3)
		if err != nil {
			return ml.Trial{}, err
		}
		fmt.Printf("Fitness=%.4f (Acc=%.2f%%, Time=%.2fms, Hashes=%d)\n",
			trial.Fitness, trial.Accuracy*100.0, trial.AvgQueryTime, trial.HashCount)
		evaluated[p] = trial
		trials = append(trials, trial)
		return trial, nil
	}

	fmt.Println("\n--- Evaluating Initial Exploratory Points ---")
	for _, p := range initialPoints {
		if _, err := evaluatePoint(p); err != nil {
			fmt.Printf("Evaluation failed for initial point %+v: %v\n", p, err)
			os.Exit(1)
		}
	}

	// surrogate optimization loop
	kappa := 1.5

	for iter := 1; iter <= *maxIterations; iter++ {
		fmt.Printf("\n--- Surrogate Optimization Iteration %d/%d ---\n", iter, *maxIterations)

		k := len(trials)
		A := make([][]float64, k)
		Y := make([]float64, k)
		for i := 0; i < k; i++ {
			A[i] = make([]float64, k)
			Y[i] = trials[i].Fitness
			for j := 0; j < k; j++ {
				pI := ml.Params{TimeStart: trials[i].TimeStart, TimeEnd: trials[i].TimeEnd, FreqBins: trials[i].FreqBins}
				pJ := ml.Params{TimeStart: trials[j].TimeStart, TimeEnd: trials[j].TimeEnd, FreqBins: trials[j].FreqBins}
				A[i][j] = ml.RBF(ml.Distance(pI, pJ))
			}
		}

		weights, err := ml.SolveLinearSystem(A, Y)
		if err != nil {
			fmt.Printf("Surrogate system singular, continuing search. Error: %v\n", err)
		}

		var bestAcquisition float64 = -math.MaxFloat64
		var nextPoint ml.Params
		nextPointFound := false

		for _, p := range allPoints {
			if _, ok := evaluated[p]; ok {
				continue
			}

			pred := 0.0
			minDist := math.MaxFloat64
			for i := 0; i < k; i++ {
				pI := ml.Params{TimeStart: trials[i].TimeStart, TimeEnd: trials[i].TimeEnd, FreqBins: trials[i].FreqBins}
				dist := ml.Distance(p, pI)
				if dist < minDist {
					minDist = dist
				}
				if weights != nil {
					pred += weights[i] * ml.RBF(dist)
				} else {
					pred += trials[i].Fitness / float64(k)
				}
			}

			uncertainty := minDist
			acq := pred + kappa*uncertainty

			if acq > bestAcquisition {
				bestAcquisition = acq
				nextPoint = p
				nextPointFound = true
			}
		}

		if !nextPointFound {
			fmt.Println("All points in neighborhood evaluated.")
			break
		}

		if _, err := evaluatePoint(nextPoint); err != nil {
			fmt.Printf("Evaluation failed for candidate point %+v: %v\n", nextPoint, err)
			os.Exit(1)
		}
	}

	// Select best trial
	var bestTrial ml.Trial
	bestTrial.Fitness = -math.MaxFloat64
	for _, t := range trials {
		if t.Fitness > bestTrial.Fitness {
			bestTrial = t
		}
	}

	fmt.Println("\n====================================================")
	fmt.Println("           NEIGHBORHOOD SEARCH SUMMARY")
	fmt.Println("====================================================")
	fmt.Printf("Best Local Parameters Found:\n")
	fmt.Printf("  TimeStart        : %d\n", bestTrial.TimeStart)
	fmt.Printf("  TimeEnd          : %d\n", bestTrial.TimeEnd)
	fmt.Printf("  FreqBins         : %d\n", bestTrial.FreqBins)
	fmt.Printf("  Fitness          : %.4f\n", bestTrial.Fitness)
	fmt.Printf("  Accuracy         : %.2f%%\n", bestTrial.Accuracy*100.0)
	fmt.Printf("  Avg Query Time   : %.2f ms\n", bestTrial.AvgQueryTime)
	fmt.Printf("  Hash Count       : %d\n", bestTrial.HashCount)

	resultsPath := filepath.Join(*workspaceDir, "optimization_results.json")
	resBytes, err := json.MarshalIndent(ml.OptResults{BestTrial: bestTrial, Trials: trials}, "", "  ")
	if err == nil {
		_ = os.WriteFile(resultsPath, resBytes, 0644)
		fmt.Printf("\nResults written to %s\n", resultsPath)
	}

	fmt.Println("🎉 Local fitness evaluation completed!")
}