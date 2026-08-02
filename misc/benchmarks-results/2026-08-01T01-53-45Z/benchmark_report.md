# Audio Fingerprint Benchmark Report

## Executive Summary
Automated CI benchmark suite executing accuracy, database, and concurrency workloads.

## Latency Metrics
| Stage | Mean | P95 | P99 |
|---|---|---|---|
| End-To-End Latency (ms) | 768.42 | 2164.51 | 2660.16 |
| Spectrogram Duration (ms) | 4.30 | 10.03 | 19.71 |
| Peak-Finding Duration (ms) | 1.02 | 3.13 | 4.45 |
| Hash Generation Duration (ms) | 0.12 | 0.23 | 0.90 |
| Hash Entropy (bits) | 7.49 | 0.00 | 0.00 |
| Hash Collisions | 2.00 | 0.00 | 0.00 |
| API Latency (1 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (10 users) ms | 1.10 | 2.00 | 2.00 |
| API Latency (100 users) ms | 0.62 | 2.00 | 3.01 |
| API Latency (500 users) ms | 10.34 | 16.00 | 24.00 |

## Accuracy
| Target | Top-1 | F1 Score |
|---|---|---|
| Noise 30dB SNR | 100.0% | 100.00 |
| Noise 20dB SNR | 100.0% | 100.00 |
| Noise 15dB SNR | 100.0% | 100.00 |
| Noise 10dB SNR | 100.0% | 100.00 |
| Noise 5dB SNR | 100.0% | 100.00 |
| Noise 0dB SNR | 0.0% | 0.00 |
| Compression (Simulated 128kbps) | 100.0% | 100.00 |
| Compression (Simulated 64kbps) | 100.0% | 100.00 |
| Speed 0.95x | 0.0% | 0.00 |
| Speed 0.98x | 0.0% | 0.00 |
| Speed 1.02x | 0.0% | 0.00 |
| Speed 1.05x | 0.0% | 0.00 |
| Clip Length 1s | 100.0% | 100.00 |
| Clip Length 2s | 100.0% | 100.00 |
| Clip Length 3s | 100.0% | 100.00 |
| Clip Length 5s | 100.0% | 100.00 |
