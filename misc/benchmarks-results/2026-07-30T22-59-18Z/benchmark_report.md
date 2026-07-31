# Audio Fingerprint Benchmark Report

## Executive Summary
Automated CI benchmark suite executing accuracy, database, and concurrency workloads.

## Latency Metrics
| Stage | Mean | P95 | P99 |
|---|---|---|---|
| End-To-End Latency (ms) | 4844.02 | 22246.20 | 33117.89 |
| Hash Entropy (bits) | 9.97 | 0.00 | 0.00 |
| Hash Collisions | 22.00 | 0.00 | 0.00 |
| API Latency (1 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (10 users) ms | 0.70 | 1.00 | 1.00 |
| API Latency (100 users) ms | 0.06 | 0.00 | 1.02 |
| API Latency (500 users) ms | 21.23 | 40.00 | 45.00 |

## Accuracy
| Target | Top-1 | F1 Score |
|---|---|---|
| Noise 30dB SNR | 100.0% | 100.00 |
| Noise 20dB SNR | 100.0% | 100.00 |
| Noise 15dB SNR | 100.0% | 100.00 |
| Noise 10dB SNR | 100.0% | 100.00 |
| Noise 5dB SNR | 100.0% | 100.00 |
| Noise 0dB SNR | 100.0% | 100.00 |
| Compression (Simulated 128kbps) | 100.0% | 100.00 |
| Compression (Simulated 64kbps) | 100.0% | 100.00 |
| Speed 0.95x | 0.0% | 0.00 |
| Speed 0.98x | 100.0% | 100.00 |
| Speed 1.02x | 0.0% | 0.00 |
| Speed 1.05x | 0.0% | 0.00 |
| Clip Length 1s | 100.0% | 100.00 |
| Clip Length 2s | 100.0% | 100.00 |
| Clip Length 3s | 100.0% | 100.00 |
| Clip Length 5s | 100.0% | 100.00 |
