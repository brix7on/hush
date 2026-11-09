package aec

import (
	"math/rand"
	"testing"
)

// Задержка наводки и её сила меняются посреди работы: подстройка должна это подхватить,
// а определение голоса — отличать тебя от друга.
func TestSupAdaptAndVAD(t *testing.T) {
	const sr = 48000
	rng := rand.New(rand.NewSource(11))
	calN := 7 * sr
	runN := 70 * sr
	n := calN + runN
	ref := make([]float64, n)
	copy(ref, bursts(calN, sr, 0.05, rng))
	friend := speech(rand.New(rand.NewSource(12)), runN, 0.25, 0)
	scaleTo(friend, 0.1)
	user := speech(rand.New(rand.NewSource(13)), runN, 0.25, 0)
	scaleTo(user, 0.02)
	userOn := func(i int) bool { sec := i / sr; return sec >= 55 && sec < 65 || (sec >= 30 && sec < 34) }
	for i := 0; i < runN; i++ {
		ref[calN+i] = friend[i]
	}
	leakA := envLeak(ref, 55*sr/1000, 0.02, rand.New(rand.NewSource(14)))
	leakB := envLeak(ref, 66*sr/1000, 0.035, rand.New(rand.NewSource(14))) // позже и сильнее
	mic := make([]float64, n)
	us := make([]float64, n)
	for i := range mic {
		l := leakA[i]
		if i >= calN+20*sr {
			l = leakB[i]
		}
		mic[i] = l + 0.0003*rng.NormFloat64()
		if i >= calN && userOn(i-calN) {
			us[i] = user[i-calN]
			mic[i] += us[i]
		}
	}
	run := func(adapt bool) (red float64, d0, d1 int, vOn, vOff float64) {
		s := NewSuppressor(sr)
		s.Adapt = adapt
		H := s.H
		out := make([]float64, n)
		rb, mb, ob := make([]float32, H), make([]float32, H), make([]float32, H)
		var onN, onV, offN, offV int
		step := 0
		for st := 0; st+H <= n; st += H {
			if step == 2 {
				s.StartCalib((calN - 4*H) / H)
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
			if st/H == (calN+10*sr)/H {
				d0 = s.Calib().D
			}
			if rel := st - calN; rel > 10*sr && !s.Calibrating() {
				if userOn(rel) {
					onN++
					if s.Voice() {
						onV++
					}
				} else if rel < 30*sr || rel > 36*sr { // вне речи (с запасом на «хвост»)
					offN++
					if s.Voice() {
						offV++
					}
				}
			}
			step++
		}
		d1 = s.Calib().D
		a, b := calN+40*sr, calN+54*sr // друг говорит, ты молчишь, после смены наводки
		red = dB(power(mic, a, b)) - dB(power(out, a, b))
		return red, d0, d1, float64(onV) / float64(onN+1), float64(offV) / float64(offN+1)
	}
	r0, _, _, _, _ := run(false)
	r1, d0, d1, vOn, vOff := run(true)
	t.Logf("после смены силы и задержки наводки: без подстройки %.1f дБ, с подстройкой %.1f дБ; задержка кадров %d → %d (было 55 мс ≈ 10, стало 66 мс ≈ 12)", r0, r1, d0, d1)
	t.Logf("голос определён: когда ты говоришь %.0f%%, когда молчишь %.0f%%", vOn*100, vOff*100)
	if r1 < r0+1 {
		t.Errorf("подстройка не помогла")
	}
	if vOn < 0.6 || vOff > 0.15 {
		t.Errorf("определение голоса не работает")
	}
}

func (s *Suppressor) Calibrating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calLeft > 0 || s.calReq > 0
}
