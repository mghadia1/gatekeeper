## Latency

| Target | Rate (req/s) | p50 ms (pass 1, 2) | p99 ms (pass 1, 2) | Mean p50 | Mean p99 | Achieved req/s | Non-200 | Dropped | Valid |
|---|---|---|---|---|---|---|---|---|---|
| direct | 500 | 0.82, 1.19 | 2.78, 2.61 | 1.01 | 2.70 | 500.0 | 0 | 0 | yes |
| direct | 2000 | 1.09, 1.00 | 9.49, 6.68 | 1.05 | 8.09 | 1999.9 | 0 | 0 | yes |
| direct | 4000 | 0.90, 0.74 | 10.69, 7.03 | 0.82 | 8.86 | 3999.0 | 0 | 0 | yes |
| gateway | 500 | 1.27, 1.46 | 3.74, 117.22 | 1.37 | 60.48 | 500.0 | 0 | 0 | yes |
| gateway | 2000 | 1.03, 1.05 | 3.60, 7.22 | 1.04 | 5.41 | 2000.0 | 0 | 0 | yes |
| gateway | 4000 | 0.81, 0.71 | 3.51, 7.24 | 0.76 | 5.38 | 3999.6 | 0 | 0 | yes |
| gateway+limit | 500 | 1.60, 1.54 | 4.15, 20.95 | 1.57 | 12.55 | 500.0 | 0 | 0 | yes |
| gateway+limit | 2000 | 1.01, 0.89 | 3.31, 3.21 | 0.95 | 3.26 | 2000.0 | 0 | 0 | yes |
| gateway+limit | 4000 | 0.37, 0.37 | 9.05, 2.29 | 0.37 | 5.67 | 4000.0 | 0 | 0 | yes |

## Rate limit accuracy (50/s, burst 50, 300/s offered for 10 s, two replicas)

- pass 1: 549 allowed, 2451 refused, 0 errors; expected 550 (-0.18%); valid: yes
- pass 2: 549 allowed, 2451 refused, 0 errors; expected 550 (-0.18%); valid: yes
