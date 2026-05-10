package store

import (
	"math"
	"testing"
)

// TestCalculateCost pins the dollar amounts the brief specifies so a
// future tweak to the rate sheet doesn't silently shift the bill on
// every running tenant.
func TestCalculateCost(t *testing.T) {
	cases := []struct {
		name                            string
		vcpuSec, memMBSec, diskGBSec    int64
		want                            float64
	}{
		{
			name:    "zero",
			want:    0,
		},
		{
			// 1 vCPU for 1h = $0.06
			name:    "1_vcpu_hour",
			vcpuSec: 3600,
			want:    0.06,
		},
		{
			// 1 GB memory for 1h = 1024 MB * 3600s = 3686400 MB-sec = $0.01
			name:     "1_gb_memory_hour",
			memMBSec: 1024 * 3600,
			want:     0.01,
		},
		{
			// 1 GB disk for 1h = $0.005
			name:      "1_gb_disk_hour",
			diskGBSec: 3600,
			want:      0.005,
		},
		{
			// Combined: 2 vCPU + 4 GB + 20 GB disk for 30 min.
			// vCPU:  2 * 1800 = 3600 vcpu-sec  → $0.06
			// Mem:   4 * 1024 * 1800 = 7372800 MB-sec → $0.02
			// Disk: 20 * 1800 = 36000 GB-sec  → $0.05
			// Total = $0.13
			name:      "combined_30m",
			vcpuSec:   3600,
			memMBSec:  4 * 1024 * 1800,
			diskGBSec: 36000,
			want:      0.13,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CalculateCost(c.vcpuSec, c.memMBSec, c.diskGBSec)
			// Allow a tiny float epsilon — these multiplications should be
			// exact at this scale, but be defensive against ULP wobble.
			if math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("cost: got %f want %f", got, c.want)
			}
		})
	}
}
