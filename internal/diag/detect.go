// Package diag — проверка «звук из вывода возвращается во вход».
package diag

import "math"

const (
	FrameMs  = 20
	PeriodMs = 800 // 400 мс шипения, 400 мс тишины
	OnMs     = 400
	Periods  = 6
	MaxLagMs = 780
)

// Tone генерирует тестовый сигнал: чередование шума и тишины.
func Tone(rate int, amp float64) []float32 {
	n := rate * PeriodMs / 1000 * Periods
	out := make([]float32, n)
	var s uint32 = 12345
	var lp float64
	for i := range out {
		t := i * 1000 / rate
		if t%PeriodMs < OnMs {
			s = s*1664525 + 1013904223
			w := float64(int32(s)) / 2147483648.0
			lp += 0.5 * (w - lp) // мягче на слух
			out[i] = float32(amp * lp * 1.7)
		}
	}
	return out
}

// Result — слышит ли вход тестовый сигнал.
type Result struct {
	Hears  bool
	R      float64 // корреляция огибающей с шаблоном
	DiffDB float64 // насколько громче во время шипения, дБ
	LagMs  int
}

// Detect: rec — запись входа (моно) на частоте rate; toneAt — через сколько мс после начала записи начал играть.

func Detect(rec []float32, rate int, toneAtMs int) Result {
	fr := rate * FrameMs / 1000
	nf := len(rec) / fr
	if nf == 0 {
		return Result{}
	}
	le := make([]float64, nf)
	for k := 0; k < nf; k++ {
		var e float64
		for _, v := range rec[k*fr : (k+1)*fr] {
			e += float64(v) * float64(v)
		}
		le[k] = 10 * math.Log10(e/float64(fr)+1e-14)
	}
	best := Result{R: -2}
	toneFrames := PeriodMs * Periods / FrameMs
	for lag := 0; lag <= MaxLagMs; lag += FrameMs {
		start := (toneAtMs + lag) / FrameMs
		if start < 0 || start+toneFrames > nf {
			continue
		}
		var on, off []float64
		var xs, ps []float64
		for k := 0; k < toneFrames; k++ {
			ms := k * FrameMs
			ph := ms % PeriodMs
			// пропускаем кадры на границах (переходы)
			if ph < FrameMs || (ph >= OnMs && ph < OnMs+FrameMs) {
				continue
			}
			v := le[start+k]
			p := 0.0
			if ph < OnMs {
				p = 1
				on = append(on, v)
			} else {
				off = append(off, v)
			}
			xs = append(xs, v)
			ps = append(ps, p)
		}
		r := corr(xs, ps)
		if r > best.R {
			best = Result{R: r, DiffDB: mean(on) - mean(off), LagMs: lag}
		}
	}
	best.Hears = best.R > 0.6 && best.DiffDB > 3
	return best
}

func mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func corr(x, y []float64) float64 {
	mx, my := mean(x), mean(y)
	var sxy, sxx, syy float64
	for i := range x {
		a, b := x[i]-mx, y[i]-my
		sxy += a * b
		sxx += a * a
		syy += b * b
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}
