package aec

import (
	"math"
	"math/rand"
	"os"
	"testing"
)

// runSup прогоняет подавитель шагами по 256; возвращает выход, выровненный с входом
// (задержка 256 отсчётов убрана).
func runSup(s *Suppressor, ref, mic []float64, calibFrom, calibFrames int) []float64 {
	H := s.H
	n := len(mic)
	out := make([]float64, n)
	rb, mb, ob := make([]float32, H), make([]float32, H), make([]float32, H)
	step := 0
	for st := 0; st+H <= n; st += H {
		if calibFrames > 0 && step == calibFrom {
			s.StartCalib(calibFrames)
		}
		for i := 0; i < H; i++ {
			rb[i], mb[i] = float32(ref[st+i]), float32(mic[st+i])
		}
		s.Process(rb, mb, ob)
		for i := 0; i < H; i++ {
			if j := st + i - (s.N - s.H); j >= 0 {
				out[j] = float64(ob[i])
			}
		}
		step++
	}
	return out
}

func TestSupIdentity(t *testing.T) {
	s := NewSuppressor(48000)
	rng := rand.New(rand.NewSource(1))
	n := 48000
	mic := make([]float64, n)
	ref := make([]float64, n)
	for i := range mic {
		mic[i] = 0.1 * rng.NormFloat64()
		ref[i] = 0.1 * rng.NormFloat64()
	}
	out := runSup(s, ref, mic, 0, 0)
	var e float64
	for i := 2048; i < n-2048; i++ {
		d := out[i] - mic[i]
		e += d * d
	}
	if e > 1e-12 {
		t.Fatalf("без калибровки выход должен совпадать с микрофоном, ошибка %g", e)
	}
}

// наводка «по огибающей»: шум, модулированный огибающей звука наушников, с задержкой
func envLeak(ref []float64, delay int, gain float64, rng *rand.Rand) []float64 {
	n := len(ref)
	env := make([]float64, n)
	var e float64
	for i := range ref {
		v := math.Abs(ref[i])
		e += 0.004 * (v - e)
		env[i] = e
	}
	out := make([]float64, n)
	var lp1, lp2 float64
	for i := range out {
		w := rng.NormFloat64()
		lp1 += 0.3 * (w - lp1)
		lp2 += 0.05 * (lp1 - lp2)
		bp := lp1 - lp2 // ~300..3000 Гц
		if j := i - delay; j >= 0 {
			out[i] = gain * env[j] * bp * 6
		}
	}
	return out
}

func bursts(n, rate int, amp float64, rng *rand.Rand) []float64 {
	out := make([]float64, n)
	for i := range out {
		if (i*1000/rate)%800 < 400 {
			out[i] = amp * rng.NormFloat64()
		}
	}
	return out
}

func TestSupEnvelopeLeak(t *testing.T) {
	const sr = 48000
	rng := rand.New(rand.NewSource(7))
	calN := 7 * sr
	runN := 30 * sr
	n := calN + runN
	ref := make([]float64, n)
	copy(ref, bursts(calN, sr, 0.05, rng))
	friend := speech(rand.New(rand.NewSource(8)), runN, 0.25, 0)
	scaleTo(friend, 0.1)
	user := speech(rand.New(rand.NewSource(9)), runN, 0.25, 0)
	scaleTo(user, 0.02)
	userOn := make([]bool, runN)
	for i := 0; i < runN; i++ {
		sec := i / sr
		// друг говорит 0..20 с; ты — 10..14 (поверх) и 22..28 (один)
		if sec < 20 {
			ref[calN+i] = friend[i]
		}
		userOn[i] = (sec >= 10 && sec < 14) || (sec >= 22 && sec < 28)
	}
	leak := envLeak(ref, 55*sr/1000, 0.02, rng)
	mic := make([]float64, n)
	us := make([]float64, n)
	for i := range mic {
		mic[i] = leak[i] + 0.0003*rng.NormFloat64()
		if i >= calN && userOn[i-calN] {
			us[i] = user[i-calN]
			mic[i] += us[i]
		}
	}
	s := NewSuppressor(sr)
	out := runSup(s, ref, mic, 2, (calN-4*s.H)/s.H)
	c := s.Calib()
	t.Logf("калибровка: найдено=%v задержка=%.0f мс корреляция=%.2f наводка над шумом=%.1f дБ",
		c.Found, float64(c.D*s.H)*1000/sr, c.Corr, c.LeakDB)
	seg := func(x []float64, a, b float64) float64 { return power(x, calN+int(a*sr), calN+int(b*sr)) }
	red := dB(seg(mic, 2, 10)) - dB(seg(out, 2, 10))
	t.Logf("только друг (ты молчишь): наводка тише на %.1f дБ", red)
	// ты один: должно остаться как было
	alone := dB(seg(out, 22, 28)) - dB(seg(mic, 22, 28))
	t.Logf("говоришь только ты: изменение %+.2f дБ", alone)
	// поверх друг друга: твой голос сохраняется?
	var ps, pe, pl float64
	for i := calN + 10*sr; i < calN+14*sr; i++ {
		ps += us[i] * us[i]
		d := out[i] - us[i]
		pe += d * d
		pl += leak[i] * leak[i]
	}
	t.Logf("поверх: голос/помеха было %.1f дБ, стало %.1f дБ; твой голос по громкости %+.1f дБ",
		dB(ps/pl), dB(ps/pe), dB(seg(out, 10, 14))-dB(seg(mic, 10, 14)))
	voice := dB(seg(out, 10, 14)) - dB(seg(mic, 10, 14))
	if !c.Found || red < 8 || math.Abs(alone) > 0.5 || voice < -2 {
		t.Errorf("не прошло")
	}
}

// Проверка на твоей записи с криком (если файл есть): калибровка на 12..17 с, проверка 17..22 с.
func TestSupRealRecording(t *testing.T) {
	p := os.Getenv("REC_WAV")
	if p == "" {
		t.Skip("REC_WAV не задан")
	}
	mic, refs, sr := readWAV24(t, p)
	ref := make([]float64, len(mic))
	for i := range ref {
		for _, r := range refs {
			ref[i] += r[i] / float64(len(refs))
		}
	}
	s := NewSuppressor(sr)
	out := runSup(s, ref, mic, 12*sr/s.H, 5*sr/s.H)
	c := s.Calib()
	t.Logf("калибровка на крике: найдено=%v задержка=%.0f мс корреляция=%.2f наводка над шумом=%.1f дБ",
		c.Found, float64(c.D*s.H)*1000/float64(sr), c.Corr, c.LeakDB)
	for _, w := range [][2]int{{17, 22}, {0, 11}} {
		a, b := w[0]*sr, w[1]*sr
		t.Logf("%d..%d с: микрофон %.1f дБ → выход %.1f дБ", w[0], w[1], dB(power(mic, a, b)), dB(power(out, a, b)))
	}
	if os.Getenv("OUT_WAV") != "" {
		writeWAVmono(t, os.Getenv("OUT_WAV"), out, sr)
	}
}
