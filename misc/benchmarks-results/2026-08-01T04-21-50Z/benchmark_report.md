# Audio Fingerprint Benchmark Report

## Executive Summary
Automated CI benchmark suite executing accuracy, database, and concurrency workloads.

## Latency Metrics
| Stage | Mean | P95 | P99 |
|---|---|---|---|
| End-To-End Latency (ms) | 1.97 | 2.37 | 2.95 |
| Spectrogram Duration (ms) | 1.43 | 1.65 | 2.27 |
| Peak-Finding Duration (ms) | 0.40 | 0.52 | 0.59 |
| Hash Generation Duration (ms) | 0.03 | 0.05 | 0.07 |
| Hash Entropy (bits) | 7.32 | 0.00 | 0.00 |
| Hash Collisions | 0.00 | 0.00 | 0.00 |
| API Latency (1 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (10 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (100 users) ms | 0.06 | 1.00 | 1.00 |
| API Latency (500 users) ms | 0.03 | 0.00 | 1.00 |

## Accuracy
| Target | Top-1 | F1 Score |
|---|---|---|
| Noise 30dB SNR | 0.0% | 0.00 |
| Noise 20dB SNR | 0.0% | 0.00 |
| Noise 15dB SNR | 0.0% | 0.00 |
| Noise 10dB SNR | 0.0% | 0.00 |
| Noise 5dB SNR | 0.0% | 0.00 |
| Noise 0dB SNR | 0.0% | 0.00 |
| Compression (Simulated 128kbps) | 0.0% | 0.00 |
| Compression (Simulated 64kbps) | 0.0% | 0.00 |
| Speed 0.95x | 0.0% | 0.00 |
| Speed 0.98x | 0.0% | 0.00 |
| Speed 1.02x | 0.0% | 0.00 |
| Speed 1.05x | 0.0% | 0.00 |
| Clip Length 1s | 0.0% | 0.00 |
| Clip Length 2s | 0.0% | 0.00 |
| Clip Length 3s | 0.0% | 0.00 |
| Clip Length 5s | 0.0% | 0.00 |
