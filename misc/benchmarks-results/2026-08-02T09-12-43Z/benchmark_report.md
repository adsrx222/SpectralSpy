# Audio Fingerprint Benchmark Report

## Executive Summary
Automated CI benchmark suite executing accuracy, database, and concurrency workloads.

## Latency Metrics
| Stage | Mean | P95 | P99 |
|---|---|---|---|
| End-To-End Latency (ms) | 4.80 | 7.08 | 9.16 |
| Spectrogram Duration (ms) | 3.03 | 4.53 | 7.72 |
| Peak-Finding Duration (ms) | 1.09 | 1.52 | 2.77 |
| Hash Generation Duration (ms) | 0.10 | 0.15 | 0.28 |
| Hash Entropy (bits) | 9.58 | 0.00 | 0.00 |
| Hash Collisions | 7.00 | 0.00 | 0.00 |
| API Latency (1 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (10 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (100 users) ms | 1.42 | 4.00 | 5.01 |
| API Latency (500 users) ms | 10.37 | 20.00 | 25.00 |

## Accuracy
| Target | Top-1 | F1 Score |
|---|---|---|
| Noise 30dB SNR | 36.0% | 36.00 |
| Noise 20dB SNR | 28.0% | 28.00 |
| Noise 15dB SNR | 20.0% | 20.00 |
| Noise 10dB SNR | 16.0% | 16.00 |
| Noise 5dB SNR | 12.0% | 12.00 |
| Noise 0dB SNR | 16.0% | 16.00 |
| Compression (Simulated 128kbps) | 32.0% | 32.00 |
| Compression (Simulated 64kbps) | 32.0% | 32.00 |
| Clip Length 1s | 16.0% | 16.00 |
| Clip Length 2s | 28.0% | 28.00 |
| Clip Length 3s | 40.0% | 40.00 |
| Clip Length 5s | 48.0% | 48.00 |