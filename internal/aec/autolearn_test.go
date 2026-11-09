package aec

import (
	"math"
	"math/rand"
	"testing"
)

// музыка-заменитель: шум с медленно гуляющей громкостью
func musicLike(n, sr int, rng *rand.Rand) []float64 {
	out := make([]float64, n)
	env, target := 0.1, 0.1
	for i := range out {
		if i%(sr/8) == 0 {
			target = 0.02 + 0.3*rng.Float64()
		}
		env += 0.0005 * (target - env)
		out[i] = env * rng.NormFloat64()
	}
	return out
}

func TestAutoLearnWithoutCalibration(t *testing.T) {
	sr := 48000
	rng := rand.New(rand.NewSource(3))
	n := 20 * sr
	ref := musicLike(n, sr, rng)
	leak := envLeak(ref, sr*50/1000, 0.05, rng)
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = leak[i] + 2e-4*rng.NormFloat64()
	}
	s := NewSuppressor(sr)
	out := runSup(s, ref, mic, 0, 0)
	if !s.cal.Found {
		t.Fatal("модель не выучилась")
	}
	if d := float64(s.cal.D*s.H) * 1000 / float64(sr); math.Abs(d-50) > 12 {
		t.Fatalf("задержка %.0f мс, ожидалось ~50", d)
	}
	in, res := power(mic, 15*sr, 20*sr), power(out, 15*sr, 20*sr)
	if red := dB(in) - dB(res); red < 8 {
		t.Fatalf("наводка убрана только на %.1f дБ", red)
	}
}

func TestAutoLearnKeepsVoice(t *testing.T) {
	sr := 48000
	rng := rand.New(rand.NewSource(4))
	n := 20 * sr
	ref := musicLike(n, sr, rng)
	leak := envLeak(ref, sr*50/1000, 0.05, rng)
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = leak[i] + 2e-4*rng.NormFloat64()
		if i > 16*sr && i < 18*sr {
			mic[i] += 0.1 * math.Sin(2*math.Pi*220*float64(i)/float64(sr)) * (1 + 0.5*math.Sin(2*math.Pi*3*float64(i)/float64(sr)))
		}
	}
	s := NewSuppressor(sr)
	out := runSup(s, ref, mic, 0, 0)
	in, res := power(mic, 16*sr+sr/4, 18*sr-sr/4), power(out, 16*sr+sr/4, 18*sr-sr/4)
	if loss := dB(in) - dB(res); loss > 3 {
		t.Fatalf("голос ослаблен на %.1f дБ", loss)
	}
}
