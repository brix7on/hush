package aec

import (
	"math"
	"sync/atomic"
)

// Denoiser — шумодав: убирает стационарный фон (вентилятор, шипение микрофона, гул).

type Denoiser struct {
	N, H     int
	strength atomic.Int32 // 0 — выкл, 1 — лёгкий, 2 — средний, 3 — сильный

	f       *fft
	win     []float64
	in      []float64
	olap    []float64
	dly     []float64 // вход, задержанный на N-H (для режима «выкл»)
	X       []complex128
	ps      []float64 // сглаженный спектр мощности
	noise   []float64
	subMin  [][]float64 // минимумы подокон
	cur     []float64   // минимум текущего подокна
	subCnt  int
	subIdx  int
	gPrev   []float64
	pPrev   []float64
	g       []float64
	inited  bool
	hasHint bool
	hang    int
	hint    atomic.Uint64
	gate    float64
	frames  int
	raw     []float32
	mix     float32
}

const (
	dnSub    = 8  // подокон
	dnSubLen = 48 // кадров в подокне (~0.13 с) → окно минимума ≈ 1 с
)

func NewDenoiser() *Denoiser {
	N, H := 256, 128
	d := &Denoiser{N: N, H: H, f: newFFT(N), gate: 1, mix: 1}
	d.win = make([]float64, N)
	for i := range d.win {
		d.win[i] = math.Sqrt(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(N)))
	}
	nb := N/2 + 1
	d.in = make([]float64, N)
	d.olap = make([]float64, N)
	d.dly = make([]float64, N-H)
	d.X = make([]complex128, N)
	d.ps = make([]float64, nb)
	d.noise = make([]float64, nb)
	d.cur = make([]float64, nb)
	d.gPrev = make([]float64, nb)
	d.pPrev = make([]float64, nb)
	d.g = make([]float64, nb)
	d.subMin = make([][]float64, dnSub)
	for i := range d.subMin {
		d.subMin[i] = make([]float64, nb)
		for k := range d.subMin[i] {
			d.subMin[i][k] = math.Inf(1)
		}
	}
	for k := range d.cur {
		d.cur[k] = math.Inf(1)
		d.gPrev[k] = 1
	}
	return d
}

// SetStrength: 0 выкл, 1 лёгкий, 2 средний, 3 сильный, 4 максимум.
func (d *Denoiser) SetStrength(s int) {
	if s < 0 {
		s = 0
	}
	if s > 4 {
		s = 4
	}
	d.strength.Store(int32(s))
}
func (d *Denoiser) Strength() int { return int(d.strength.Load()) }

func (d *Denoiser) floorDB() float64 {
	switch d.strength.Load() {
	case 1:
		return -9
	case 2:
		return -15
	case 3:
		return -26
	default:
		return -36
	}
}

// параметры по силе: переоценка шума (злее вычитание) и глубина гейта в паузах, дБ
func (d *Denoiser) params() (over, gateDB float64) {
	switch d.strength.Load() {
	case 3:
		return 1.4, -18
	case 4:
		return 1.8, -40
	}
	return 1, 0
}

// Process обрабатывает на месте кусок длиной, кратной H (например, блок движка 256).

func (d *Denoiser) Process(x []float32) {
	H := d.H
	if len(d.raw) != H {
		d.raw = make([]float32, H)
	}
	for i := 0; i+H <= len(x); i += H {
		seg := x[i : i+H]
		copy(d.raw, seg)
		d.hop(seg)
		target := float32(0)
		if d.strength.Load() != 0 {
			target = 1
		}
		if d.mix == target {
			if target == 0 {
				copy(seg, d.raw)
			}
			continue
		}
		step := 1 / float32(H)
		for j := range seg {
			if d.mix < target {
				d.mix = min(target, d.mix+step)
			} else if d.mix > target {
				d.mix = max(target, d.mix-step)
			}
			seg[j] = d.mix*seg[j] + (1-d.mix)*d.raw[j]
		}
	}
}

// Delay — сколько отсчётов задержки сейчас вносит шумодав.
func (d *Denoiser) Delay() int {
	if d.strength.Load() == 0 {
		return 0
	}
	return d.N - d.H
}

func (d *Denoiser) hop(x []float32) {
	N, H := d.N, d.H
	copy(d.in, d.in[H:])
	for i := 0; i < H; i++ {
		d.in[N-H+i] = float64(x[i])
	}
	for i := 0; i < N; i++ {
		d.X[i] = complex(d.in[i]*d.win[i], 0)
	}
	d.f.forward(d.X)
	nb := N/2 + 1

	// спектр, шум
	const eps = 1e-14
	for k := 0; k < nb; k++ {
		p := real(d.X[k])*real(d.X[k]) + imag(d.X[k])*imag(d.X[k])
		if !d.inited {
			d.ps[k] = p
		} else {
			d.ps[k] = 0.84*d.ps[k] + 0.16*p
		}
		if d.ps[k] < d.cur[k] {
			d.cur[k] = d.ps[k]
		}
	}
	d.subCnt++
	if d.subCnt >= dnSubLen {
		copy(d.subMin[d.subIdx], d.cur)
		d.subIdx = (d.subIdx + 1) % dnSub
		for k := range d.cur {
			d.cur[k] = math.Inf(1)
		}
		d.subCnt = 0
	}
	for k := 0; k < nb; k++ {
		m := d.cur[k]
		for i := 0; i < dnSub; i++ {
			if d.subMin[i][k] < m {
				m = d.subMin[i][k]
			}
		}
		if math.IsInf(m, 1) {
			m = d.ps[k]
		}
		n := m * 3.0
		if !d.inited {
			d.noise[k] = n
		} else if n > d.noise[k] {
			d.noise[k] += 0.025 * (n - d.noise[k]) // шум растёт медленно
		} else {
			d.noise[k] = n
		}
	}
	d.inited = true
	d.frames++

	st := d.strength.Load()
	gmin := math.Pow(10, d.floorDB()/20)
	over, gateDB := d.params()

	var sp, sn float64
	for k := 1; k < nb && k <= 21; k++ { // ~190–4000 Гц
		sp += p2(d.X[k])
		sn += d.noise[k]
	}
	speech := sp > 2.2*sn+eps
	if h := math.Float64frombits(d.hint.Load()); h >= 0 && d.hasHint {

		speech = speech && h > 0.25
	}
	if speech {
		d.hang = 80 // ~0.2 с держим открытым после речи
	} else if d.hang > 0 {
		d.hang--
	}
	gateT := 1.0
	if gateDB < 0 && d.hang == 0 {
		gateT = math.Pow(10, gateDB/20)
	}
	if gateT > d.gate {
		d.gate += 0.3 * (gateT - d.gate) // открываемся быстро (~8 мс)
	} else {
		d.gate += 0.02 * (gateT - d.gate) // закрываемся плавно (~130 мс)
	}
	for k := 0; k < nb; k++ {
		lam := over*d.noise[k] + eps
		p := real(d.X[k])*real(d.X[k]) + imag(d.X[k])*imag(d.X[k])
		gamma := p / lam
		xi := 0.9*d.gPrev[k]*d.gPrev[k]*d.pPrev[k]/lam + 0.1*math.Max(gamma-1, 0)
		if xi < 1e-3 {
			xi = 1e-3
		}
		g := xi / (1 + xi)
		d.g[k] = g
	}

	for k := 0; k < nb; k++ {
		a, b, c := d.g[maxInt(k-1, 0)], d.g[k], d.g[minInt(k+1, nb-1)]
		g := 0.25*a + 0.5*b + 0.25*c
		if g < d.gPrev[k]*0.92 {
			g = d.gPrev[k] * 0.92
		}
		if g < gmin {
			g = gmin
		}
		if g > 1 {
			g = 1
		}
		d.pPrev[k] = p2(d.X[k])
		d.gPrev[k] = g
		d.g[k] = g
	}
	if st == 0 {
		for k := 0; k < nb; k++ {
			d.g[k] = 1
		}
	} else if d.gate < 0.999 {
		for k := 0; k < nb; k++ {
			d.g[k] *= d.gate
		}
	}
	for k := 0; k < nb; k++ {
		d.X[k] *= complex(d.g[k], 0)
		if k > 0 && k < N/2 {
			d.X[N-k] *= complex(d.g[k], 0)
		}
	}
	d.f.inverse(d.X)
	for i := 0; i < N; i++ {
		d.olap[i] += real(d.X[i]) * d.win[i]
	}
	for i := 0; i < H; i++ {
		v := d.olap[i]
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		x[i] = float32(v)
	}
	copy(d.olap, d.olap[H:])
	for i := N - H; i < N; i++ {
		d.olap[i] = 0
	}
}

func p2(c complex128) float64 { return real(c)*real(c) + imag(c)*imag(c) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// VoiceHint передаёт вероятность «сейчас говоришь» (0..1) от подавителя наводки.
func (d *Denoiser) VoiceHint(v float64) {
	d.hasHint = true
	d.hint.Store(math.Float64bits(v))
}
