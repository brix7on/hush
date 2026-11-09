package diag

import (
	"math/rand"
	"testing"
)

func mk(rate, toneAt, delayMs int, gain, noise float64) []float32 {
	tone := Tone(rate, 0.1)
	n := rate * (toneAt + delayMs + 6000) / 1000
	rec := make([]float32, n)
	rng := rand.New(rand.NewSource(1))
	off := rate * (toneAt + delayMs) / 1000
	for i := range rec {
		rec[i] = float32(noise * rng.NormFloat64())
		if j := i - off; j >= 0 && j < len(tone) {
			rec[i] += float32(gain) * tone[j]
		}
	}
	return rec
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		gain  float64
		delay int
		want  bool
	}{
		{"петля через кабель (громко, 30 мс)", 1, 30, true},
		{"тихая утечка, но выше шума (250 мс)", 0.05, 250, true},
		{"микрофон без утечки", 0, 100, false},
		{"утечка глубоко под шумом", 0.0005, 100, false},
	}
	for _, c := range cases {
		r := Detect(mk(48000, 500, c.delay, c.gain, 0.0005), 48000, 500)
		t.Logf("%s: слышит=%v r=%.2f разница=%.1f дБ задержка=%d мс", c.name, r.Hears, r.R, r.DiffDB, r.LagMs)
		if r.Hears != c.want {
			t.Errorf("%s: ожидалось %v", c.name, c.want)
		}
	}
	// другой частоты устройство
	r := Detect(mk(44100, 300, 60, 0.3, 0.001), 44100, 300)
	if !r.Hears {
		t.Errorf("44.1 кГц: не нашёл")
	}
}
