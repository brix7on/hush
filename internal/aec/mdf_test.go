package aec

import (
	"math"
	"math/rand"
	"testing"
)

const sr = 48000

// «Речь»: окрашенный шум с медленной амплитудной модуляцией и паузами.
func speech(rng *rand.Rand, n int, amp, tone float64) []float64 {
	out := make([]float64, n)
	var lp1, lp2 float64
	for i := range out {
		w := rng.NormFloat64()
		lp1 += 0.25 * (w - lp1)
		lp2 += 0.6 * (lp1 - lp2)
		t := float64(i) / sr
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3.1*t)
		env *= 0.5 + 0.5*math.Sin(2*math.Pi*0.37*t+1)
		out[i] = amp * env * (lp1*2 + lp2*3 + tone*math.Sin(2*math.Pi*220*t))
	}
	return out
}

func conv(x, h []float64) []float64 {
	y := make([]float64, len(x))
	for i := range x {
		var s float64
		for k, hk := range h {
			if i-k >= 0 {
				s += hk * x[i-k]
			}
		}
		y[i] = s
	}
	return y
}

func echoPath(delay int, gain float64, rng *rand.Rand) []float64 {
	h := make([]float64, delay+2400)
	for k := 0; k < 2400; k++ {
		h[delay+k] = gain * rng.NormFloat64() * math.Exp(-float64(k)/500)
	}
	h[delay] = gain
	return h
}

func power(x []float64, a, b int) float64 {
	var s float64
	for i := a; i < b; i++ {
		s += x[i] * x[i]
	}
	return s / float64(b-a)
}

func run(t *testing.T, delay int) {
	rng := rand.New(rand.NewSource(1))
	secs := 24
	n := secs * sr
	ref := speech(rng, n, 0.25, 0.3)
	h := echoPath(delay, 0.35, rng)
	echo := conv(ref, h)
	near := make([]float64, n)
	nz := speech(rand.New(rand.NewSource(7)), n, 0.2, 0)
	for i := 10 * sr; i < 15*sr; i++ { // double-talk: 10..15 c
		near[i] = nz[i]
	}
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = echo[i] + near[i] + 0.0005*rng.NormFloat64()
	}

	f := New(Config{})
	B := f.B
	out := make([]float64, n)
	rb, mb, ob := make([]float32, B), make([]float32, B), make([]float32, B)
	for s := 0; s+B <= n; s += B {
		for i := 0; i < B; i++ {
			rb[i], mb[i] = float32(ref[s+i]), float32(mic[s+i])
		}
		f.Process([][]float32{rb}, mb, ob)
		for i := 0; i < B; i++ {
			out[s+i] = float64(ob[i])
		}
	}
	att := func(a, b int) float64 {
		return 10 * math.Log10(power(mic, a*sr, b*sr)/power(out, a*sr, b*sr))
	}
	a1, a2 := att(6, 10), att(18, 24)
	t.Logf("delay=%dмс: подавление 6-10с: %.1f дБ, 18-24с: %.1f дБ", delay*1000/sr, a1, a2)
	if a1 < 25 || a2 < 25 {
		t.Errorf("слабое подавление: %.1f / %.1f дБ", a1, a2)
	}
	// double-talk: голос сохранился? (мощность near vs. остаток out-near)
	var errP float64
	for i := 11 * sr; i < 15*sr; i++ {
		d := out[i] - near[i]
		errP += d * d
	}
	errP /= float64(4 * sr)
	snr := 10 * math.Log10(power(near, 11*sr, 15*sr)/errP)
	t.Logf("double-talk: речь/искажения = %.1f дБ", snr)
	if snr < 10 {
		t.Errorf("речь искажена: %.1f дБ", snr)
	}
}

func TestEcho25ms(t *testing.T) { run(t, 1200) }
func TestEcho90ms(t *testing.T) { run(t, 4320) }
func TestSilentRef(t *testing.T) {
	f := New(Config{})
	B := f.B
	ref, mic, out := make([]float32, B), make([]float32, B), make([]float32, B)
	for i := range mic {
		mic[i] = float32(0.3 * math.Sin(float64(i)*0.1))
	}
	for k := 0; k < 100; k++ {
		f.Process([][]float32{ref}, mic, out)
		for i := range out {
			if math.Abs(float64(out[i]-mic[i])) > 1e-6 {
				t.Fatalf("при тишине в наушниках микрофон должен проходить без изменений")
			}
		}
	}
}
