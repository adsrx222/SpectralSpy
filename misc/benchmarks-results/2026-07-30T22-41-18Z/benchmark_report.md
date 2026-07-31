# Audio Fingerprint Benchmark Report

## Executive Summary
Automated CI benchmark suite executing accuracy, database, and concurrency workloads.

## Latency Metrics
| Stage | Mean | P95 | P99 |
|---|---|---|---|
| End-To-End Latency (ms) | 5640.92 | 8026.75 | 8928.85 |
| Hash Entropy (bits) | 9.32 | 0.00 | 0.00 |
| Hash Collisions | 9.00 | 0.00 | 0.00 |
| API Latency (1 users) ms | 0.00 | 0.00 | 0.00 |
| API Latency (10 users) ms | 0.90 | 2.00 | 2.00 |
| API Latency (100 users) ms | 0.03 | 0.00 | 1.01 |
| API Latency (500 users) ms | 0.06 | 0.00 | 2.00 |

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
| Speed 0.98x | 0.0% | 0.00 |
| Speed 1.02x | 0.0% | 0.00 |
| Speed 1.05x | 0.0% | 0.00 |
| Clip Length 1s | 100.0% | 100.00 |
| Clip Length 2s | 100.0% | 100.00 |
| Clip Length 3s | 100.0% | 100.00 |
| Clip Length 5s | 100.0% | 100.00 |
