package aec

import (
	"math"
	"math/rand"
	"testing"
)

func scaleTo(x []float64, rms float64) {
	var s float64
	for _, v := range x {
		s += v * v
	}
	k := rms / math.Sqrt(s/float64(len(x)))
	for i := range x {
		x[i] *= k
	}
}

func dB(p float64) float64 { return 10 * math.Log10(p+1e-30) }

// runK прогоняет фильтр с заданными опорами; возвращает выход.
func runK(f *Filter, refs [][]float64, mic []float64) []float64 {
	B := f.B
	n := len(mic)
	out := make([]float64, n)
	rb := make([][]float32, len(refs))
	for k := range rb {
		rb[k] = make([]float32, B)
	}
	mb, ob := make([]float32, B), make([]float32, B)
	for s := 0; s+B <= n; s += B {
		for k := range refs {
			for i := 0; i < B; i++ {
				rb[k][i] = float32(refs[k][s+i])
			}
		}
		for i := 0; i < B; i++ {
			mb[i] = float32(mic[s+i])
		}
		f.Process(rb, mb, ob)
		for i := 0; i < B; i++ {
			out[s+i] = float64(ob[i])
		}
	}
	return out
}

// Наводка идёт ТОЛЬКО от левого наушника (микрофон у левой чашки), а L и R — разная музыка.
// Моно-опора (L+R)/2 это не описывает; две раздельные опоры — описывают.
func TestStereoCoupling(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	n := 18 * sr
	rng := rand.New(rand.NewSource(11))
	L := speech(rng, n, 0.25, 0.3)
	R := speech(rand.New(rand.NewSource(12)), n, 0.25, 0.3)
	scaleTo(L, 0.1)
	scaleTo(R, 0.1)
	h := echoPath(1500, 0.2, rng)
	echo := conv(L, h)
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = echo[i] + 0.0005*rng.NormFloat64()
	}
	mono := make([]float64, n)
	for i := range mono {
		mono[i] = (L[i] + R[i]) / 2
	}
	att := func(out []float64) float64 {
		return dB(power(mic, 12*sr, 18*sr)) - dB(power(out, 12*sr, 18*sr))
	}
	a2 := att(runK(New(Config{Refs: 2}), [][]float64{L, R}, mic))
	a1 := att(runK(New(Config{Refs: 1}), [][]float64{mono}, mic))
	t.Logf("две опоры L/R: %.1f дБ | одна моно-опора: %.1f дБ", a2, a1)
	if a2 < 25 {
		t.Errorf("две опоры: мало %.1f дБ", a2)
	}
	if a2 < a1+10 {
		t.Errorf("стерео должно быть заметно лучше моно: %.1f vs %.1f", a2, a1)
	}
}

// В микрофоне только шум, в наушниках громко: фильтр не должен добавлять шум.
func TestNoiseOnlyNoHarm(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	n := 40 * sr
	rng := rand.New(rand.NewSource(21))
	ref := speech(rng, n, 0.25, 0.3)
	scaleTo(ref, 0.1)
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = 0.0008 * rng.NormFloat64()
	}
	f := New(Config{})
	out := runK(f, [][]float64{ref}, mic)
	r := power(out, 10*sr, 40*sr) / power(mic, 10*sr, 40*sr)
	t.Logf("только шум: выход/микрофон = %+.2f дБ, срабатываний предохранителя: %d", dB(r), f.Stats().Resets)
	if r > 1.5 {
		t.Errorf("фильтр добавил шум: %+.2f дБ", dB(r))
	}
}

// Как у тебя: наводка на ~40 дБ ниже наушников, почти на уровне шума микрофона.
func TestWeakEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	n := 40 * sr
	rng := rand.New(rand.NewSource(31))
	L := speech(rng, n, 0.25, 0.3)
	R := speech(rand.New(rand.NewSource(32)), n, 0.25, 0.3)
	scaleTo(L, 0.1)
	scaleTo(R, 0.1)
	h := echoPath(1800, 0.01, rng) // -40 дБ
	echo := conv(L, h)
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = echo[i] + 0.0008*rng.NormFloat64()
	}
	f := New(Config{Refs: 2})
	out := runK(f, [][]float64{L, R}, mic)
	a := dB(power(mic, 20*sr, 40*sr)) - dB(power(out, 20*sr, 40*sr))
	best := dB(power(mic, 20*sr, 40*sr)) - dB(0.0008*0.0008)
	t.Logf("слабая наводка: подавление %.1f дБ (теоретический максимум ~%.1f дБ), предохранитель: %d", a, best, f.Stats().Resets)
	if a < 0.6*best {
		t.Errorf("слабая наводка почти не подавляется: %.1f из %.1f дБ", a, best)
	}
}

func BenchmarkProcess2(b *testing.B) {
	f := New(Config{Refs: 2})
	rng := rand.New(rand.NewSource(1))
	r := [][]float32{make([]float32, f.B), make([]float32, f.B)}
	m, o := make([]float32, f.B), make([]float32, f.B)
	for i := range m {
		r[0][i], r[1][i], m[i] = float32(rng.NormFloat64()*0.1), float32(rng.NormFloat64()*0.1), float32(rng.NormFloat64()*0.1)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Process(r, m, o)
	}
}
