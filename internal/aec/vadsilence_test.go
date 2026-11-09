package aec

import (
	"math"
	"math/rand"
	"testing"
)

// Тишина в наушниках, ты молчишь, микрофон шумит неровно (как у USB-гарнитуры):
// «ты говоришь» гореть не должно. А обычная речь — должна.
func TestVADSilence(t *testing.T) {
	const sr = 48000
	rng := rand.New(rand.NewSource(5))
	n := 20 * sr
	mic := make([]float64, n)
	ref := make([]float64, n)
	var lp float64
	for i := range mic {
		lp += 0.1 * (rng.NormFloat64() - lp)
		mod := 1 + 0.45*math.Sin(2*math.Pi*0.7*float64(i)/sr) // неровный шум (±4 дБ)
		mic[i] = 0.0008 * mod * (lp*2 + 0.5*rng.NormFloat64())
	}
	sp := speech(rand.New(rand.NewSource(6)), n, 0.25, 0)
	scaleTo(sp, 0.03)
	s := NewSuppressor(sr)
	H := s.H
	rb, mb, ob := make([]float32, H), make([]float32, H), make([]float32, H)
	var silV, silN, spV, spN int
	for st := 0; st+H <= n; st += H {
		talk := st >= 12*sr && st < 16*sr
		for i := 0; i < H; i++ {
			rb[i] = float32(ref[st+i])
			v := mic[st+i]
			if talk {
				v += sp[st+i]
			}
			mb[i] = float32(v)
		}
		s.Process(rb, mb, ob)
		if st > 3*sr && st < 11*sr {
			silN++
			if s.Voice() {
				silV++
			}
		}
		if talk && st > 12*sr+sr/2 {
			spN++
			if s.Voice() {
				spV++
			}
		}
	}
	t.Logf("тишина: «говоришь» %d%%; речь: %d%%", 100*silV/silN, 100*spV/spN)
	if silV*100/silN > 5 || spV*100/spN < 50 {
		t.Errorf("не прошло")
	}
}
