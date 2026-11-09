package aec

import (
	"math/rand"
	"testing"
)

func TestEstimateDelay(t *testing.T) {
	for _, lagMs := range []int{-60, 5, 40, 120} {
		rng := rand.New(rand.NewSource(5))
		n := 16384
		ref := speech(rng, n*4, 0.2, 0.1)
		lag := lagMs * 12 // отсчётов при 12 кГц
		r := make([]float32, n)
		m := make([]float32, n)
		for i := 0; i < n; i++ {
			r[i] = float32(ref[i*4])
			src := i - lag
			var v float64
			if src >= 0 && src < n {
				v = 0.3 * ref[src*4]
			}
			m[i] = float32(v + 0.01*rng.NormFloat64())
		}
		got, conf := EstimateDelay(r, m, 2640)
		t.Logf("истинная задержка %d отсчётов, найдено %d, уверенность %.1f", lag, got, conf)
		if got < lag-2 || got > lag+2 || conf < 8 {
			t.Errorf("плохая оценка")
		}
	}
	// без связи — уверенность низкая
	rng := rand.New(rand.NewSource(8))
	a := make([]float32, 16384)
	b := make([]float32, 16384)
	for i := range a {
		a[i], b[i] = float32(rng.NormFloat64()), float32(rng.NormFloat64())
	}
	_, conf := EstimateDelay(a, b, 2640)
	t.Logf("несвязанные сигналы: уверенность %.1f", conf)
	if conf >= 8 {
		t.Errorf("ложное срабатывание")
	}
}
