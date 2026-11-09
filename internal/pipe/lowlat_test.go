package pipe

import (
	"math"
	"testing"
)

// Микрофон приходит пачками по 10 мс, вывод опрашивается раз в 1 мс.
func TestSinkLowLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	for _, tgt := range []int{10, 12, 15} {
		for _, drift := range []float64{300e-6, -300e-6} {
			s := NewSink(48000, tgt)
			padding := 0.0
			under := 0
			minF := math.MaxFloat64
			burst := make([]float32, 480)
			for ms := 0; ms < 120000; ms++ {
				if ms%10 == 0 {
					s.Feed(burst)
				}
				p := int(padding)
				if got := s.Take(p, 4800-p); len(got) > 0 {
					padding += float64(len(got))
				}
				padding -= 48 * (1 + drift)
				if padding < 0 {
					if ms > 1000 {
						under++
					}
					padding = 0
				}
				if ms > 2000 {
					minF = math.Min(minF, padding)
				}
			}
			t.Logf("цель %d мс, дрейф %+.0f ppm: провалов %d, минимум в буфере %.1f мс", tgt, drift*1e6, under, minF/48)
			if under > 0 {
				t.Errorf("провалы")
			}
		}
	}
}
