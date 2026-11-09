package aec

import (
	"fmt"
	"math"
	"os"
	"testing"
)

// synthVoice — грубая «речь»: гармоники 140 Гц с формантами, слоговая огибающая 4 Гц.
func synthVoice(n, rate int, rmsDB float64) []float64 {
	v := make([]float64, n)
	for i := range v {
		t := float64(i) / float64(rate)
		var s float64
		for h := 1; h <= 40; h++ {
			f := 140 * float64(h)
			if f > 5000 {
				break
			}
			a := math.Exp(-math.Pow((f-600)/400, 2)) + 0.7*math.Exp(-math.Pow((f-1800)/700, 2)) + 0.1
			s += a * math.Sin(2*math.Pi*f*t+float64(h))
		}
		env := math.Max(0, math.Sin(2*math.Pi*2.5*t))
		v[i] = s * env
	}
	p := power(v, 0, n)
	k := math.Pow(10, rmsDB/20) / math.Sqrt(p)
	for i := range v {
		v[i] *= k
	}
	return v
}

func TestVoiceKept(t *testing.T) {
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
	if v := os.Getenv("MICSCALE"); v != "" {
		var f float64
		fmt.Sscan(v, &f)
		for i := range mic {
			mic[i] *= f
		}
	}
	mk := func() *Suppressor {
		s := NewSuppressor(sr)
		if v := os.Getenv("BETA"); v != "" {
			var f float64
			if _, e := sscanf(v, &f); e == nil {
				s.SetBeta(f)
			}
		}
		if v := os.Getenv("FLOOR"); v != "" {
			var f float64
			if _, e := sscanf(v, &f); e == nil {
				s.FloorDB = f
			}
		}
		if os.Getenv("NOADAPT") != "" {
			s.Adapt = false
		}
		return s
	}
	for _, vdb := range []float64{-50, -42, -34} {
		v := synthVoice(len(mic), sr, vdb)
		mix := make([]float64, len(mic))
		for i := range mic {
			mix[i] = mic[i] + v[i]
		}
		s1, s2 := mk(), mk()
		o1 := runSup(s1, ref, mic, 12*sr/s1.H, 5*sr/s1.H)
		o2 := runSup(s2, ref, mix, 12*sr/s2.H, 5*sr/s2.H)
		// сравниваем только после калибровки и в местах, где в наушниках громко
		var pv, pd, pl, pm float64
		var cnt int
		for a := 18 * sr; a+sr/10 < len(mic); a += sr / 10 {
			b := a + sr/10
			if dB(power(ref, a, b)) < -45 {
				continue
			}
			d := make([]float64, b-a)
			for i := range d {
				d[i] = o2[a+i] - o1[a+i]
			}
			pv += power(v, a, b)
			pd += power(d, 0, len(d))
			pl += power(o1, a, b)
			pm += power(mic, a, b)
			cnt++
		}
		if cnt == 0 {
			t.Skip("нет громких окон")
		}
		t.Logf("голос %.0f дБ: сохранилось %.1f дБ от исходного | наводка: мик %.1f → вых %.1f дБ (окон %d)",
			vdb, dB(pd)-dB(pv), dB(pm/float64(cnt)), dB(pl/float64(cnt)), cnt)
	}
}

func sscanf(s string, f *float64) (int, error) {
	var x float64
	n, err := fmtSscan(s, &x)
	*f = x
	return n, err
}
func fmtSscan(s string, x *float64) (int, error) { return fmt.Sscan(s, x) }
