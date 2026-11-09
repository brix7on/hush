package aec

import (
	"math"
	"math/rand"
	"testing"
)

func runDn(d *Denoiser, x []float64) []float64 {
	H := d.H
	out := make([]float64, len(x))
	buf := make([]float32, H)
	for st := 0; st+H <= len(x); st += H {
		for i := 0; i < H; i++ {
			buf[i] = float32(x[st+i])
		}
		d.Process(buf)
		for i := 0; i < H; i++ {
			if j := st + i - (d.N - d.H); j >= 0 {
				out[j] = float64(buf[i])
			}
		}
	}
	return out
}

func TestDenoise(t *testing.T) {
	const sr = 48000
	rng := rand.New(rand.NewSource(3))
	n := 30 * sr
	sp := speech(rand.New(rand.NewSource(4)), n, 0.25, 0)
	scaleTo(sp, 0.05)
	noise := make([]float64, n)
	var lp float64
	for i := range noise {
		lp += 0.2 * (rng.NormFloat64() - lp) // розоватый фон
		noise[i] = 0.004 * (lp*3 + 0.3*rng.NormFloat64())
	}
	on := func(i int) bool { s := i / sr; return s >= 10 && s < 14 || s >= 20 && s < 26 }
	mix := make([]float64, n)
	for i := range mix {
		mix[i] = noise[i]
		if on(i) {
			mix[i] += sp[i]
		}
	}
	for _, lvl := range []int{1, 2, 3, 4} {
		d := NewDenoiser()
		d.SetStrength(lvl)
		out := runDn(d, mix)
		gap := dB(power(mix, 2*sr, 9*sr)) - dB(power(out, 2*sr, 9*sr))
		// речь: сравниваем (выход − шум-только) с исходной речью
		var pd, ps float64
		for i := 10 * sr; i < 14*sr; i++ {
			e := out[i] - 0
			_ = e
		}
		sig := make([]float64, n)
		for i := range sig {
			if on(i) {
				sig[i] = sp[i]
			}
		}
		speechOut := dB(power(out, 10*sr, 14*sr)) - dB(power(mix, 10*sr, 14*sr))
		for i := 10 * sr; i < 14*sr; i++ {
			d := out[i] - sig[i]
			pd += d * d
			ps += sig[i] * sig[i]
		}
		t.Logf("сила %d: пауза тише на %.1f дБ; речь по громкости %+.1f дБ; SNR выхода %.1f дБ (был %.1f)",
			lvl, gap, speechOut, 10*math.Log10(ps/pd), dB(power(sig, 10*sr, 14*sr))-dB(power(noise, 10*sr, 14*sr)))
		if gap < []float64{0, 5, 9, 20, 30}[lvl] || speechOut < -2.5 {
			t.Errorf("сила %d: не прошло", lvl)
		}
	}
	// трудный случай: речь всего на ~6 дБ громче шума
	{
		sp2 := make([]float64, n)
		copy(sp2, sp)
		for i := range sp2 {
			sp2[i] *= 0.12
		}
		mix2 := make([]float64, n)
		sig2 := make([]float64, n)
		for i := range mix2 {
			mix2[i] = noise[i]
			if on(i) {
				mix2[i] += sp2[i]
				sig2[i] = sp2[i]
			}
		}
		for _, lv := range []int{2, 4} {
			d := NewDenoiser()
			d.SetStrength(lv)
			out := runDn(d, mix2)
			var pd, ps float64
			for i := 10 * sr; i < 14*sr; i++ {
				e := out[i] - sig2[i]
				pd += e * e
				ps += sig2[i] * sig2[i]
			}
			in := dB(power(sig2, 10*sr, 14*sr)) - dB(power(noise, 10*sr, 14*sr))
			o := 10 * math.Log10(ps/pd)
			t.Logf("трудный случай, сила %d: SNR вход %.1f дБ → выход %.1f дБ; речь %+.1f дБ", lv, in, o,
				dB(power(out, 10*sr, 14*sr))-dB(power(mix2, 10*sr, 14*sr)))
			need := 2.0
			if lv == 4 {
				need = 1 // «Макс» честно режет и тихую речь — тут важна тишина в паузах
			}
			if o < in+need {
				t.Errorf("на тихой речи шумодав не улучшил SNR")
			}
		}
	}
	// выключенный — вход без изменений и без задержки
	d := NewDenoiser()
	d.SetStrength(0)
	out := make([]float64, len(mix))
	buf := make([]float32, d.H)
	for st := 0; st+d.H <= len(mix); st += d.H {
		for i := range buf {
			buf[i] = float32(mix[st+i])
		}
		d.Process(buf)
		for i := range buf {
			out[st+i] = float64(buf[i])
		}
	}
	if d.Delay() != 0 {
		t.Errorf("выключенный шумодав сообщает задержку %d", d.Delay())
	}
	var e float64
	for i := 2 * sr; i < 8*sr; i++ {
		e += math.Abs(out[i] - mix[i])
	}
	if e/float64(6*sr) > 1e-5 {
		t.Errorf("выключенный шумодав меняет сигнал: %g", e/float64(6*sr))
	}
}

// Переключение шумодава на ходу не должно щёлкать (резкий скачок сигнала).
func TestDenoiserToggleSmooth(t *testing.T) {
	d := NewDenoiser()
	d.SetStrength(0)
	const sr = 48000
	buf := make([]float32, 256)
	prev := float32(0)
	maxJump := 0.0
	n := 0
	for blk := 0; blk < 400; blk++ {
		switch blk {
		case 100:
			d.SetStrength(2)
		case 200:
			d.SetStrength(0)
		case 300:
			d.SetStrength(1)
		}
		for i := range buf {
			buf[i] = float32(0.3 * math.Sin(2*math.Pi*300*float64(n)/sr))
			n++
		}
		d.Process(buf)
		for _, v := range buf {
			if blk > 2 {
				maxJump = math.Max(maxJump, math.Abs(float64(v-prev)))
			}
			prev = v
		}
	}
	// у синуса 300 Гц амплитудой 0.3 соседние отсчёты отличаются не больше ~0.012
	if maxJump > 0.05 {
		t.Errorf("щелчок при переключении: скачок %.3f", maxJump)
	}
	t.Logf("наибольший скачок между отсчётами: %.4f", maxJump)
}
