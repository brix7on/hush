package pipe

import (
	"math"
	"math/rand"
	"testing"
)

func lvlDB(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return 10 * math.Log10(s/float64(len(x))+1e-12)
}

func TestAGC(t *testing.T) {
	a := NewAGC()
	a.SetEnabled(true)
	rng := rand.New(rand.NewSource(1))
	mk := func(amp float64, n int) []float32 {
		x := make([]float32, n)
		for i := range x {
			x[i] = float32(amp * math.Sin(2*math.Pi*300*float64(i)/Rate) * (0.6 + 0.4*math.Sin(2*math.Pi*3*float64(i)/Rate)))
		}
		return x
	}
	run := func(x []float32) []float32 {
		out := make([]float32, len(x))
		copy(out, x)
		for i := 0; i+256 <= len(out); i += 256 {
			a.Process(out[i : i+256])
		}
		return out
	}
	// тихая речь (−42 дБ) 10 секунд
	q := mk(0.008, 10*Rate)
	oq := run(q)
	tail := len(oq) - 2*Rate
	in1, out1 := lvlDB(q[tail:]), lvlDB(oq[tail:])
	t.Logf("тихая речь: вход %.1f → выход %.1f дБ (цель −23), усиление %.1f дБ", in1, out1, a.GainDB())
	if out1 < in1+8 || out1 > -17 {
		t.Errorf("тихая речь не подтянута")
	}
	// громкая речь (−10 дБ) должна быть приглушена
	l := mk(0.4, 6*Rate)
	ol := run(l)
	tail = len(ol) - Rate
	t.Logf("громкая речь: вход %.1f → выход %.1f дБ", lvlDB(l[tail:]), lvlDB(ol[tail:]))
	if lvlDB(ol[tail:]) > lvlDB(l[tail:])-2 {
		t.Errorf("громкая речь не приглушена")
	}
	// тишина с шумом не усиливается
	a = NewAGC()
	a.SetEnabled(true)
	nz := make([]float32, 10*Rate)
	for i := range nz {
		nz[i] = float32(0.0006 * rng.NormFloat64())
	}
	on := run(nz)
	t.Logf("фон: вход %.1f → выход %.1f дБ", lvlDB(nz), lvlDB(on))
	if lvlDB(on) > lvlDB(nz)+1 {
		t.Errorf("фон усилен")
	}
}

func TestEQ(t *testing.T) {
	gain := func(preset int, f float64) float64 {
		e := NewEQ()
		e.SetPreset(preset)
		n := Rate
		x := make([]float32, n)
		for i := range x {
			x[i] = float32(0.1 * math.Sin(2*math.Pi*f*float64(i)/Rate))
		}
		for i := 0; i+256 <= n; i += 256 {
			e.Process(x[i : i+256])
		}
		return lvlDB(x[Rate/2:]) - lvlDB(func() []float32 {
			y := make([]float32, n/2)
			for i := range y {
				y[i] = float32(0.1 * math.Sin(2*math.Pi*f*float64(i)/Rate))
			}
			return y
		}())
	}
	for _, p := range []int{EQClear, EQSoft} {
		t.Logf("пресет %d: 50 Гц %+.1f | 250 Гц %+.1f | 1 кГц %+.1f | 3.2 кГц %+.1f | 6 кГц %+.1f | 12 кГц %+.1f дБ",
			p, gain(p, 50), gain(p, 250), gain(p, 1000), gain(p, 3200), gain(p, 6000), gain(p, 12000))
	}
	if g := gain(EQOff, 1000); math.Abs(g) > 0.01 {
		t.Errorf("выкл меняет сигнал: %g", g)
	}
	if g := gain(EQClear, 50); g > -6 {
		t.Errorf("срез НЧ слабый: %g", g)
	}
	if g := gain(EQClear, 3200); g < 1.5 {
		t.Errorf("подъём разборчивости слабый: %g", g)
	}
}
