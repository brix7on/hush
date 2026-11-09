// Package aec — адаптивный фильтр для вычитания «наводки» из микрофона.
//
// Алгоритм: блочный частотный адаптивный фильтр с разбиением на секции
// (partitioned-block FDAF, overlap-save) с несколькими опорными каналами
// (например, левый и правый наушник по отдельности). Длинный «хвост»
// (по умолчанию 24 секции по 256 отсчётов = ~128 мс при 48 кГц) покрывает и
// задержку драйверов/буферов, и акустику между наушниками и микрофоном.
package aec

import (
	"math"
	"math/cmplx"
)

const (
	DefaultBlock      = 256
	DefaultPartitions = 24
	refThreshold      = 1e-9
	warmupBlocks      = 600
	stuckBlocks       = 2250
	minRatio          = 0.02

	// предохранитель от расходящегося фильтра

	badRatio  = 1.5
	badBlocks = 40
)

type Config struct {
	Block      int     // размер блока (степень двойки), по умолчанию 256
	Partitions int     // число секций, по умолчанию 24
	Refs       int     // число опорных каналов, по умолчанию 1
	Mu         float64 // шаг адаптации (0..1), по умолчанию 0.3
}

type Stats struct {
	MicDB, RefDB, OutDB float64 // уровни, дБFS (усреднённые)
	ERLEdB              float64
	Resets              int // сколько раз сработал предохранитель
}

type Filter struct {
	B, N, P, K int
	Mu         float64

	f      *fft
	w, xs  [][][]complex128 // [опора][секция][бин]
	head   int
	xbuf   [][]float64
	pn     []float64
	acc    []complex128
	ebuf   []complex128
	g      []complex128
	e      []float64
	active int

	// сглаженные мощности
	py, pe     float64
	sMic, sRef float64
	sOut       float64
	sD, sO     float64
	ns, nmin   float64
	stuck      int // сколько блоков подряд адаптация заморожена

	gPd, gPe float64 // для предохранителя
	bad      int
	resets   int
	muScale  float64
}

func New(c Config) *Filter {
	if c.Block == 0 {
		c.Block = DefaultBlock
	}
	if c.Partitions == 0 {
		c.Partitions = DefaultPartitions
	}
	if c.Refs == 0 {
		c.Refs = 1
	}
	if c.Mu == 0 {
		c.Mu = 0.3
	}
	B, N, P, K := c.Block, 2*c.Block, c.Partitions, c.Refs
	f := &Filter{B: B, N: N, P: P, K: K, Mu: c.Mu, f: newFFT(N), muScale: 1}
	f.w = make([][][]complex128, K)
	f.xs = make([][][]complex128, K)
	f.xbuf = make([][]float64, K)
	for k := 0; k < K; k++ {
		f.w[k] = make([][]complex128, P)
		f.xs[k] = make([][]complex128, P)
		for p := 0; p < P; p++ {
			f.w[k][p] = make([]complex128, N)
			f.xs[k][p] = make([]complex128, N)
		}
		f.xbuf[k] = make([]float64, N)
	}
	f.pn = make([]float64, N)
	f.acc = make([]complex128, N)
	f.ebuf = make([]complex128, N)
	f.g = make([]complex128, N)
	f.e = make([]float64, B)
	return f
}

func (f *Filter) zeroWeights() {
	for k := range f.w {
		for p := range f.w[k] {
			for i := range f.w[k][p] {
				f.w[k][p][i] = 0
			}
		}
	}
}

// Reset полностью сбрасывает фильтр (веса и история).
func (f *Filter) Reset() {
	f.zeroWeights()
	for k := range f.xs {
		for p := range f.xs[k] {
			for i := range f.xs[k][p] {
				f.xs[k][p][i] = 0
			}
		}
		for i := range f.xbuf[k] {
			f.xbuf[k][i] = 0
		}
	}
	for i := range f.pn {
		f.pn[i] = 0
	}
	f.active, f.py, f.pe, f.ns, f.nmin, f.stuck = 0, 0, 0, 0, 0, 0
	f.gPd, f.gPe, f.bad, f.muScale = 0, 0, 0, 1
}

// Rewarm заново включает быструю адаптацию (веса сохраняются).
func (f *Filter) Rewarm() { f.active, f.ns, f.nmin, f.stuck = 0, 0, 0, 0 }

// Process обрабатывает один блок: refs — каналы наушников, mic — микрофон.

func (f *Filter) Process(refs [][]float32, mic, out []float32) {
	B, N, P, K := f.B, f.N, f.P, f.K

	f.head = (f.head + 1) % P
	var refE float64
	for k := 0; k < K; k++ {
		xb := f.xbuf[k]
		copy(xb[:B], xb[B:])
		var r []float32
		if k < len(refs) {
			r = refs[k]
		}
		for i := 0; i < B; i++ {
			var v float64
			if r != nil {
				v = float64(r[i])
			}
			xb[B+i] = v
			refE += v * v
		}
		X := f.xs[k][f.head]
		for i := 0; i < N; i++ {
			X[i] = complex(xb[i], 0)
		}
		f.f.forward(X)
	}
	refE /= float64(B)

	for i := range f.acc {
		f.acc[i] = 0
	}
	for k := 0; k < K; k++ {
		for p := 0; p < P; p++ {
			Xp := f.xs[k][(f.head-p+P)%P]
			Wp := f.w[k][p]
			for i := 0; i < N; i++ {
				f.acc[i] += Wp[i] * Xp[i]
			}
		}
	}
	f.f.inverse(f.acc)

	var micE, yE, eE float64
	for i := 0; i < B; i++ {
		y := real(f.acc[B+i])
		d := float64(mic[i])
		e := d - y
		f.e[i] = e
		micE += d * d
		yE += y * y
		eE += e * e
		if e > 1 {
			e = 1
		} else if e < -1 {
			e = -1
		}
		out[i] = float32(e)
	}
	micE /= float64(B)
	yE /= float64(B)
	eE /= float64(B)

	const a = 0.9
	f.sMic = a*f.sMic + (1-a)*micE
	f.sRef = a*f.sRef + (1-a)*refE
	f.sOut = a*f.sOut + (1-a)*eE
	const fa = 0.75 // быстрее реагируем на появление голоса

	if refE <= refThreshold {
		return // опоры нет — учиться нечему
	}
	f.active++
	f.sD = a*f.sD + (1-a)*micE
	f.sO = a*f.sO + (1-a)*eE
	f.py = fa*f.py + (1-fa)*yE
	f.pe = fa*f.pe + (1-fa)*eE

	// Предохранитель «не навреди».
	const ga = 0.95
	f.gPd = ga*f.gPd + (1-ga)*micE
	f.gPe = ga*f.gPe + (1-ga)*eE
	if f.active > 20 && f.gPe > badRatio*f.gPd+1e-13 {
		f.bad++
		if f.bad > badBlocks {
			f.zeroWeights()
			f.resets++
			f.bad = 0
			f.gPd, f.gPe = 0, 0
			f.muScale = math.Max(0.3, f.muScale*0.6)
			f.Rewarm()
			return
		}
	} else {
		f.bad = 0
	}

	n := f.pe / (f.py + 1e-12)
	f.ns = 0.95*f.ns + 0.05*n
	if f.nmin == 0 || f.ns < f.nmin {
		f.nmin = f.ns
	} else {
		f.nmin *= 1.001
	}
	ratio := 1.0
	if f.active > warmupBlocks {
		ratio = 6 * f.nmin / (f.ns + 1e-12)
		if ratio < minRatio {
			ratio = minRatio
		}
		if ratio > 1 {
			ratio = 1
		}

		if ratio <= minRatio*1.5 {
			f.stuck++
			if f.stuck > stuckBlocks {
				f.Rewarm()
			}
		} else {
			f.stuck = 0
		}
	}
	f.adapt(f.Mu * ratio * f.muScale)
}

func (f *Filter) adapt(mu float64) {
	B, N, P, K := f.B, f.N, f.P, f.K
	var mean float64
	for i := 0; i < N; i++ {
		var s float64
		for k := 0; k < K; k++ {
			for p := 0; p < P; p++ {
				v := f.xs[k][(f.head-p+P)%P][i]
				s += real(v)*real(v) + imag(v)*imag(v)
			}
		}
		f.pn[i] = 0.8*f.pn[i] + 0.2*s
		mean += f.pn[i]
	}
	mean /= float64(N)
	floor := 0.02*mean + 1e-9

	for i := 0; i < B; i++ {
		f.ebuf[i] = 0
		f.ebuf[B+i] = complex(f.e[i], 0)
	}
	f.f.forward(f.ebuf)

	for k := 0; k < K; k++ {
		for p := 0; p < P; p++ {
			Xp := f.xs[k][(f.head-p+P)%P]
			for i := 0; i < N; i++ {
				f.g[i] = cmplx.Conj(Xp[i]) * f.ebuf[i] * complex(mu/(f.pn[i]+floor), 0)
			}
			f.f.inverse(f.g)
			for i := B; i < N; i++ {
				f.g[i] = 0
			}
			f.f.forward(f.g)
			Wp := f.w[k][p]
			for i := 0; i < N; i++ {
				Wp[i] += f.g[i]
			}
		}
	}
}

func db(p float64) float64 {
	if p < 1e-12 {
		p = 1e-12
	}
	return 10 * math.Log10(p)
}

func (f *Filter) Stats() Stats {
	s := Stats{MicDB: db(f.sMic), RefDB: db(f.sRef), OutDB: db(f.sOut), Resets: f.resets}
	if f.sO > 0 {
		s.ERLEdB = db(f.sD) - db(f.sO)
	}
	return s
}
