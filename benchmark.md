# Benchmarking & Test Implementation

## Goal

Implement **all** benchmarks and test suites described below.

This document is the **source of truth and acceptance checklist**. Every checkbox must be completed, tested, and verified before the task is considered complete.

Do not skip, combine, or silently reinterpret requirements. If an implementation detail is ambiguous, inspect the existing codebase and follow established project conventions. If a requirement genuinely cannot be implemented, document the blocker and evidence rather than marking it complete.

---

# Part I — `benchmark.go`

## 1. Hash Survival Benchmark #1 — Noise

* [ ] Add a benchmark for hash survival under increasing noise.
* [ ] Use the standard 2-second clean audio clip as the input.
* [ ] Generate the clean audio fingerprints once.
* [ ] Start at **0% noise**.
* [ ] Increase noise by the configured fixed increment.
* [ ] For every noise level:

  * [ ] Generate/process the noisy audio.

  * [ ] Generate fingerprints for the noisy audio.

  * [ ] Compare noisy fingerprints against the clean fingerprints.

  * [ ] Calculate hash survival as:

    `matching noisy/clean fingerprints / number of clean fingerprints`

  * [ ] Calculate identification accuracy.

  * [ ] Report whether the correct song was identified.

  * [ ] Report the matching offset/margin.

  * [ ] Calculate/report hash rate.
* [ ] Continue increasing noise until hash survival falls **below 50%**.
* [ ] Make the noise increment configurable rather than hard-coded where appropriate.
* [ ] Ensure the benchmark output identifies the noise level for every measurement.
* [ ] Ensure results are deterministic/reproducible where the benchmark design permits.
* [ ] Add tests or validation for the hash-survival calculation.
* [ ] Verify the benchmark runs successfully against the standard test clip.

### Required output

For every tested noise level:

* [ ] Noise level
* [ ] Hash Survival
* [ ] Accuracy — correct song
* [ ] Accuracy — offset/margin
* [ ] Hash Rate

---

## 2. Hash Survival Benchmark #2 — Bitrate / Audio Transcoding

* [ ] Add a benchmark for hash survival after audio transcoding.

* [ ] Use the standard 2-second clean WAV audio clip.

* [ ] Generate the clean audio fingerprints once.

* [ ] Use FFmpeg to transcode the same WAV clip at each bitrate:

  * [ ] 256 kbps
  * [ ] 160 kbps
  * [ ] 96 kbps
  * [ ] 48 kbps
  * [ ] 16 kbps

* [ ] Generate fingerprints for every transcoded version.

* [ ] Compare modified fingerprints against the clean fingerprints.

* [ ] Calculate hash survival as:

  `matching modified/clean fingerprints / number of clean fingerprints`

* [ ] Calculate identification accuracy.

* [ ] Report whether the correct song was identified.

* [ ] Report matching offset/margin.

* [ ] Calculate/report hash rate.

* [ ] Run **more than 30 trials per bitrate**.

* [ ] Aggregate the repeated trials into meaningful sample data.

* [ ] Report appropriate aggregate statistics for the repeated trials.

* [ ] Ensure each result identifies its bitrate.

* [ ] Ensure FFmpeg invocation failures are detected and reported rather than silently ignored.

* [ ] Verify the benchmark runs successfully against the standard test clip.

### Required output

For every bitrate:

* [ ] Bitrate
* [ ] Number of trials
* [ ] Hash Survival
* [ ] Accuracy — correct song
* [ ] Accuracy — offset/margin
* [ ] Hash Rate
* [ ] Aggregate statistics across trials

---

## 3. Hash Survival Benchmark #3 — Dynamic Compression

* [ ] Add a benchmark for hash survival under dynamic-range compression.
* [ ] Use the standard 2-second clean WAV audio clip.
* [ ] Generate the clean audio fingerprints once.
* [ ] Use FFmpeg to apply dynamic-range compression with varying parameters.
* [ ] Test the following compression ranges:

### Low Compression — Subtle / Transparent

* [ ] Ratio: `1.5` to `2`
* [ ] Threshold: `-12 dB` to `-18 dB`

### Medium Compression — Standard / Radio

* [ ] Ratio: `3` to `5`
* [ ] Threshold: `-18 dB` to `-24 dB`

### High Compression — Aggressive / Smashed

* [ ] Ratio: `8` to `20`

* [ ] Threshold: `-24 dB` to `-40 dB`

* [ ] Define a reproducible sampling strategy across the parameter ranges.

* [ ] Generate fingerprints for every modified clip.

* [ ] Compare modified fingerprints against clean fingerprints.

* [ ] Calculate hash survival as:

  `matching modified/clean fingerprints / number of clean fingerprints`

* [ ] Calculate identification accuracy.

* [ ] Report whether the correct song was identified.

* [ ] Report matching offset/margin.

* [ ] Calculate/report hash rate.

* [ ] Run **more than 30 trials** for each relevant parameter/range.

* [ ] Aggregate the repeated trials into meaningful sample data.

* [ ] Identify the compression parameters associated with each result.

* [ ] Verify FFmpeg failures are detected and reported.

* [ ] Verify the benchmark runs successfully.

### Required output

For every compression configuration/range:

* [ ] Compression category
* [ ] Ratio
* [ ] Threshold
* [ ] Number of trials
* [ ] Hash Survival
* [ ] Accuracy — correct song
* [ ] Accuracy — offset/margin
* [ ] Hash Rate
* [ ] Aggregate statistics

---

## 4. Hash Survival Benchmark #4 — Equalization

* [ ] Add a benchmark for hash survival under EQ changes.
* [ ] Use the standard 2-second clean WAV audio clip.
* [ ] Generate the clean audio fingerprints once.
* [ ] Use FFmpeg to apply varying equalization parameters.
* [ ] Test the following EQ ranges:

### Subtle / Smooth Adjustments

* [ ] Gain: `-3 dB` to `+3 dB`
* [ ] Width type: `q:width=0.7`
* [ ] Intended behavior: broad frequency boost/cut

### Moderate / Noticeable Coloration

* [ ] Gain: `-6 dB` to `+6 dB`
* [ ] Width type: `q:width=1.4`
* [ ] Intended behavior: standard musical/speech shaping

### Extreme / Aggressive Peak Sweeps

* [ ] Gain: `-15 dB` to `+15 dB`
* [ ] Width type: `q:width=3.0`
* [ ] Intended behavior: sharp resonant peaks/deep cuts

### Surgical Notch Filtering

* [ ] Gain: `-20 dB` or lower

* [ ] Width type: `q:width=10.0+`

* [ ] Intended behavior: elimination of a highly specific frequency

* [ ] Define a reproducible sampling strategy across the parameter ranges.

* [ ] Generate fingerprints for every modified clip.

* [ ] Compare modified fingerprints against clean fingerprints.

* [ ] Calculate hash survival as:

  `matching modified/clean fingerprints / number of clean fingerprints`

* [ ] Calculate identification accuracy.

* [ ] Report whether the correct song was identified.

* [ ] Report matching offset/margin.

* [ ] Calculate/report hash rate.

* [ ] Run **more than 30 trials** for each relevant configuration/range.

* [ ] Aggregate the repeated trials into meaningful sample data.

* [ ] Identify the EQ parameters associated with each result.

* [ ] Verify FFmpeg failures are detected and reported.

* [ ] Verify the benchmark runs successfully.

### Required output

For every EQ configuration/range:

* [ ] EQ category
* [ ] Gain
* [ ] Q/width
* [ ] Number of trials
* [ ] Hash Survival
* [ ] Accuracy — correct song
* [ ] Accuracy — offset/margin
* [ ] Hash Rate
* [ ] Aggregate statistics

---

## 5. Hash Survival Benchmark #5 — Reverberation

* [ ] Add a benchmark for hash survival under reverberation.
* [ ] Use the standard 2-second clean WAV audio clip.
* [ ] Generate the clean audio fingerprints once.
* [ ] Use FFmpeg to apply varying reverberation parameters.
* [ ] Test the following ranges:

### Tight Acoustic Space

* [ ] Room size: `0.1` to `0.25`
* [ ] Wet mix: `5%` to `15%`
* [ ] Represents small rooms, cars, and office cubicles

### Diffuse Ambient Hall

* [ ] Room size: `0.4` to `0.6`
* [ ] Wet mix: `20%` to `40%`
* [ ] Represents standard interior rooms or recording studios

### Dense Cathedral Ring

* [ ] Room size: `0.75` to `0.9`
* [ ] Wet mix: `45%` to `65%`
* [ ] Represents long decay tails that mask clean vowel/consonant shapes

### Infinite Feedback Loop

* [ ] Room size: `0.95+`

* [ ] Wet mix: `70%` to `95%`

* [ ] Stress the boundary limits of the fingerprinting system

* [ ] Define a reproducible sampling strategy across the parameter ranges.

* [ ] Generate fingerprints for every modified clip.

* [ ] Compare modified fingerprints against clean fingerprints.

* [ ] Calculate hash survival as:

  `matching modified/clean fingerprints / number of clean fingerprints`

* [ ] Calculate identification accuracy.

* [ ] Report whether the correct song was identified.

* [ ] Report matching offset/margin.

* [ ] Calculate/report hash rate.

* [ ] Run **more than 30 trials** for each relevant configuration/range.

* [ ] Aggregate the repeated trials into meaningful sample data.

* [ ] Identify the reverberation parameters associated with each result.

* [ ] Verify FFmpeg failures are detected and reported.

* [ ] Verify the benchmark runs successfully.

### Required output

For every reverb configuration/range:

* [ ] Reverb category
* [ ] Room size
* [ ] Wet mix
* [ ] Number of trials
* [ ] Hash Survival
* [ ] Accuracy — correct song
* [ ] Accuracy — offset/margin
* [ ] Hash Rate
* [ ] Aggregate statistics

---

## 6. Hash Rate Benchmarking

* [ ] Add a benchmark measuring fingerprint-processing throughput.
* [ ] Start with a clean audio clip.
* [ ] Process the clip and measure fingerprint-processing duration.
* [ ] Measure processing time independently from unrelated setup overhead where possible.
* [ ] Start at a `0s` audio duration/sample size.
* [ ] Increase the processed audio duration by a configurable fixed increment.
* [ ] For each duration:

  * [ ] Run **30–40 trials**.
  * [ ] Record processing duration for every trial.
  * [ ] Calculate mean processing duration.
  * [ ] Calculate P95 processing duration.
  * [ ] Calculate P99 processing duration.
  * [ ] Calculate hashes/second.
* [ ] Report the number of hashes generated.
* [ ] Report processing duration.
* [ ] Report hash/second throughput.
* [ ] Determine whether processing time scales approximately linearly with audio duration.
* [ ] Provide enough output/data to evaluate scaling behavior rather than only reporting a single aggregate number.
* [ ] Verify the benchmark runs successfully.

### Required output

For every tested audio duration:

* [ ] Audio duration
* [ ] Trial count
* [ ] Mean processing duration
* [ ] P95 processing duration
* [ ] P99 processing duration
* [ ] Hash count
* [ ] Hashes/second

---

## 7. Hash Collision Benchmarking

* [ ] Add hash-collision/distribution benchmarking.
* [ ] Inspect the processed database.
* [ ] Read `track_count` values from the `hash_weight` table.
* [ ] Calculate the following percentiles:

  * [ ] P50
  * [ ] P95
  * [ ] P99
  * [ ] P100 / maximum
* [ ] Report the number of rows/samples analyzed.
* [ ] Ensure percentile calculations are correct for the available dataset.
* [ ] Clearly identify the maximum value separately from the percentile calculations where appropriate.
* [ ] Verify the benchmark runs successfully against a representative database.

### Required output

* [ ] Sample/row count
* [ ] P50 `track_count`
* [ ] P95 `track_count`
* [ ] P99 `track_count`
* [ ] P100 / maximum `track_count`

---

## 8. Full HTTP Load Testing

* [ ] Add/document a full HTTP load-testing workflow.
* [ ] Use the **Vegeta Go tool**.
* [ ] Identify the HTTP endpoints/workflows that represent the production workload.
* [ ] Define a reproducible Vegeta test configuration.
* [ ] Execute the load test against the appropriate test environment.
* [ ] Measure throughput.
* [ ] Measure P50 latency/throughput metric as appropriate to the existing load-test output.
* [ ] Measure P95.
* [ ] Measure P99.
* [ ] Measure error rate.
* [ ] Capture the exact load-test configuration used.
* [ ] Ensure failures/errors are visible in the results.
* [ ] Verify the load-test workflow can be reproduced by another developer.

### Required output

* [ ] Test configuration
* [ ] Request rate/load
* [ ] P50
* [ ] P95
* [ ] P99
* [ ] Throughput
* [ ] Error rate
* [ ] Test duration
* [ ] Target endpoint/workflow

---

# Part II — `spectral_spy_test.go`

## 9. Memory Leak Testing

* [ ] Add memory-leak testing for the fingerprinting/audio-processing pipeline.
* [ ] Exercise the relevant processing paths repeatedly.
* [ ] Detect meaningful memory growth across repeated runs.
* [ ] Ensure resources are released correctly.
* [ ] Ensure the test fails when memory usage demonstrates an unacceptable leak.
* [ ] Verify the test is reproducible and does not rely on arbitrary timing alone.

---

## 10. Context Cancellation & Timeout Testing

* [ ] Add tests for context cancellation.
* [ ] Verify processing observes cancelled contexts.
* [ ] Verify processing terminates promptly after cancellation.
* [ ] Add timeout tests.
* [ ] Verify operations terminate when their context deadline expires.
* [ ] Verify appropriate errors are returned.
* [ ] Verify no goroutine/resource leak occurs after cancellation or timeout.

---

## 11. Data Race Testing

* [ ] Add race-detection coverage for relevant fingerprinting/audio-processing code.
* [ ] Exercise concurrent processing paths.
* [ ] Run the relevant tests with Go's race detector.
* [ ] Resolve any race conditions discovered in the implementation.
* [ ] Ensure the race-enabled test suite passes.

---

## 12. `MatchFingerprint` Testing Suite

* [ ] Add a comprehensive `MatchFingerprint` test suite.
* [ ] Test successful matching.
* [ ] Test correct-song identification.
* [ ] Test incorrect/non-matching fingerprints.
* [ ] Test offset calculation.
* [ ] Test offset/margin behavior.
* [ ] Test multiple matching candidates where applicable.
* [ ] Test empty input.
* [ ] Test insufficient fingerprint data.
* [ ] Test malformed/invalid input where applicable.
* [ ] Test boundary conditions.
* [ ] Test behavior with duplicate/colliding hashes where applicable.
* [ ] Verify expected errors are returned.
* [ ] Verify expected match results are returned.
* [ ] Add regression tests for any bugs discovered while implementing the suite.

---

# Final Verification

## 13. Requirement Audit

* [ ] Re-read this entire document from beginning to end after implementation.
* [ ] Verify every checkbox has actually been implemented.
* [ ] Do not mark an item complete merely because code exists.
* [ ] Verify every benchmark actually executes.
* [ ] Verify benchmark outputs contain all required metrics.
* [ ] Verify repeated-trial requirements are actually satisfied.
* [ ] Verify FFmpeg-dependent benchmarks fail clearly when FFmpeg is unavailable or fails.
* [ ] Verify benchmark calculations against known/simple cases.
* [ ] Verify tests cover the requested failure and edge cases.
* [ ] Search the codebase for existing benchmark/test infrastructure and ensure the new work integrates with it rather than duplicating incompatible mechanisms.
* [ ] Run the complete relevant Go test suite.
* [ ] Run tests with the race detector where applicable.
* [ ] Run formatting.
* [ ] Run lint/static analysis available in the repository.
* [ ] Run the relevant benchmarks.
* [ ] Fix all failures caused by the implementation.
* [ ] Confirm there are no uncommitted accidental/generated files that should not be included.
* [ ] Perform one final requirement-by-requirement audit against this document.

# Part III — Benchmark Execution & Output

## 14. Runnable Benchmark Command

* [ ] Provide a simple way to run all implemented benchmarks using `go run`.
* [ ] The benchmark must be executable without requiring manual code changes.
* [ ] The benchmark command must accept an **output directory** as an argument.
* [ ] The output directory argument must be required or have a clearly documented default.
* [ ] Example usage must be documented, such as:

```bash
go run ./benchmark.go --output ./benchmark-results
```

* [ ] The implementation must create a unique **timestamped subdirectory** inside the supplied output directory for each benchmark run.
* [ ] Each benchmark execution must therefore produce its own isolated output directory.

For example:

```text
benchmark-results/
├── 2026-08-10_22-30-15/
│   ├── ...
│   └── ...
├── 2026-08-10_23-05-42/
│   ├── ...
│   └── ...
└── 2026-08-11_09-12-03/
    ├── ...
    └── ...
```

* [ ] The timestamp must have sufficient precision to prevent collisions between benchmark runs.
* [ ] Do not overwrite results from a previous benchmark run.
* [ ] Create the timestamped output directory automatically if it does not exist.
* [ ] All files generated by a single benchmark execution must be placed inside that execution's timestamped output directory.
* [ ] Do not write benchmark artifacts into the repository root unless explicitly required.
* [ ] Print the final output directory path when the benchmark starts or completes so the user can easily find the results.
* [ ] Ensure the output directory path works with both relative and absolute paths.
* [ ] Handle invalid/unwritable output directories with a clear error and non-zero exit status.
* [ ] Document the command-line arguments and their purpose.
* [ ] Document the expected output directory structure.
* [ ] Verify that running the benchmark multiple times produces separate timestamped result directories.

## 15. Benchmark Run Metadata

* [ ] Each benchmark run must record enough metadata to identify how the results were produced.
* [ ] Record the benchmark run timestamp.
* [ ] Record the relevant benchmark configuration/parameters.
* [ ] Record the number of trials performed.
* [ ] Record the input audio/test fixture used.
* [ ] Record relevant application/build information where reasonably available.
* [ ] Store this metadata inside the timestamped benchmark output directory.
* [ ] Use a machine-readable format such as JSON where appropriate.
* [ ] Ensure benchmark results can be understood without relying on terminal output from the original run.

## Definition of Done — Benchmark Execution

* [ ] A developer can run the benchmark with a single `go run` command.
* [ ] The developer can specify where benchmark results should be written.
* [ ] Every execution creates a unique timestamped subdirectory.
* [ ] Multiple runs never overwrite one another.
* [ ] All benchmark artifacts are contained within the corresponding run directory.
* [ ] The command reports where the results were written.
* [ ] The command fails clearly when the output directory cannot be created or written.
* [ ] The usage and output structure are documented.
* [ ] A second developer can reproduce a benchmark run using the documented command.
