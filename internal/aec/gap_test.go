package aec

import (
	"math"
	"math/rand"
	"testing"
)

func TestGap(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	rng := rand.New(rand.NewSource(1))
	n := 16 * sr
	ref := speech(rng, n, 0.25, 0.3)
	for i := 4 * sr; i < 7*sr; i++ {
		ref[i] = 0
	}
	h := echoPath(1200, 0.35, rng)
	echo := conv(ref, h)
	f := New(Config{})
	B := f.B
	rb, mb, ob := make([]float32, B), make([]float32, B), make([]float32, B)
	out := make([]float64, n)
	for s := 0; s+B <= n; s += B {
		for i := 0; i < B; i++ {
			rb[i], mb[i] = float32(ref[s+i]), float32(echo[s+i])
		}
		f.Process([][]float32{rb}, mb, ob)
		for i := 0; i < B; i++ {
			out[s+i] = float64(ob[i])
		}
	}
	for s := 0; s < 16; s++ {
		t.Logf("%2d с: att %.1f дБ", s, 10*math.Log10((power(echo, s*sr, (s+1)*sr)+1e-12)/(power(out, s*sr, (s+1)*sr)+1e-12)))
	}
}

// Внезапный сдвиг опоры (например, пересинхронизация) — фильтр должен восстановиться сам.
func TestShiftRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	rng := rand.New(rand.NewSource(1))
	n := 50 * sr
	ref := speech(rng, n, 0.25, 0.3)
	h := echoPath(1200, 0.35, rng)
	echo := conv(ref, h)
	f := New(Config{})
	B := f.B
	rb, mb, ob := make([]float32, B), make([]float32, B), make([]float32, B)
	out := make([]float64, n)
	const shift = 960
	for s := 0; s+B <= n; s += B {
		for i := 0; i < B; i++ {
			k := s + i
			if k >= 8*sr { // после 8 с опора приходит на 20 мс позже
				k -= shift
			}
			rb[i], mb[i] = float32(ref[k]), float32(echo[s+i])
		}
		f.Process([][]float32{rb}, mb, ob)
		for i := 0; i < B; i++ {
			out[s+i] = float64(ob[i])
		}
	}
	for _, sec := range []int{6, 10, 20, 30, 40, 48} {
		t.Logf("%2d с: att %.1f дБ", sec, 10*math.Log10((power(echo, sec*sr, (sec+1)*sr)+1e-12)/(power(out, sec*sr, (sec+1)*sr)+1e-12)))
	}
	if a := 10 * math.Log10(power(echo, 44*sr, 50*sr)/power(out, 44*sr, 50*sr)); a < 25 {
		t.Errorf("не восстановился: %.1f дБ", a)
	}
}

func BenchmarkProcess(b *testing.B) {
	f := New(Config{})
	rng := rand.New(rand.NewSource(1))
	r, m, o := make([]float32, f.B), make([]float32, f.B), make([]float32, f.B)
	for i := range r {
		r[i], m[i] = float32(rng.NormFloat64()*0.1), float32(rng.NormFloat64()*0.1)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Process([][]float32{r}, m, o)
	}
}
