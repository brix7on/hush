package aec

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// Suppressor — подавитель наводки «по огибающей».

var (
	vadNfMul = 3.0 // минимум шума занижает средний уровень — поправка
	vadStart = 6.0
	vadFull  = 16.0
)

const (
	SupFrame = 512
	SupHop   = 256
	supHist  = 80 // кадров истории опоры (~430 мс)
	supMaxD  = 64 // максимальная задержка наводки, кадров (~340 мс)
)

var supEdges = []float64{0, 100, 200, 300, 400, 510, 630, 770, 920, 1080, 1270, 1480, 1720, 2000,
	2320, 2700, 3150, 3700, 4400, 5300, 6400, 7700, 9500, 12000, 15500, 1e9}

// Calib — результат калибровки (сохраняется в файл).
type Calib struct {
	Rate   int       `json:"rate"`
	D      int       `json:"delay_frames"`
	W      []float64 `json:"w"`
	Corr   float64   `json:"corr"`
	Found  bool      `json:"found"`
	LeakDB float64   `json:"leak_db"` // насколько наводка громче шума при калибровке
	W0     []float64 `json:"w_cal,omitempty"`
}

type SupStats struct {
	MicDB, RefDB, OutDB float64
	ReductionDB         float64
	Calibrating         bool
	Voice               bool
	Found               bool
	DelayMs             float64
}

type Suppressor struct {
	Rate     int
	beta     atomic.Uint64
	FloorDB  float64 // максимальное ослабление, дБ (отрицательное)
	Adapt    bool    // медленная подстройка модели по ходу работы
	N, H, nb int

	f        *fft
	win      []float64
	bandOf   []int
	inMic    []float64
	inRef    []float64
	olap     []float64
	M, R     []complex128
	mb, rb   []float64
	S        []float64   // сглаженная опора (текущий кадр)
	hist     [][]float64 // история S
	hpos     int
	nf       []float64 // шумовой пол микрофона по полосам
	nfInit   bool
	G        []float64
	gbin     []float64
	out      []float32 // выход за последний шаг
	frameIdx int

	cal     Calib
	w0      []float64
	d0      int     // задержка на момент калибровки
	vad     float64 // 0..1: «сейчас говоришь ты», сглаженно
	vadRaw  float64
	bbFloor float64 // уровень тишины микрофона, дБ
	bbInit  bool

	adapted bool // модель менялась по ходу работы

	// калибровка
	mu       sync.Mutex
	calReq   int
	calLeft  int
	calM     [][]float64
	calS     [][]float64
	calDone  chan Calib
	muted    bool
	frames   int
	sMic     float64
	sRef     float64
	sOut     float64
	sDa, sOa float64
	pub      pubStats
}

type pubStats struct{ mic, ref, out, vad, da, oa float64 }

// SetBeta задаёт силу подавления.
func (s *Suppressor) SetBeta(b float64) { s.beta.Store(math.Float64bits(b)) }

// Beta — текущая сила подавления.
func (s *Suppressor) Beta() float64 { return math.Float64frombits(s.beta.Load()) }

func NewSuppressor(rate int) *Suppressor {
	N, H := SupFrame, SupHop
	s := &Suppressor{Rate: rate, FloorDB: -20, Adapt: true, N: N, H: H, f: newFFT(N)}
	s.SetBeta(1.5)
	s.win = make([]float64, N)
	for i := range s.win {
		s.win[i] = math.Sqrt(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(N)))
	}
	nbins := N/2 + 1
	s.bandOf = make([]int, nbins)
	s.nb = 0
	for k := 0; k < nbins; k++ {
		hz := float64(k) * float64(rate) / float64(N)
		b := 0
		for b+1 < len(supEdges) && hz >= supEdges[b+1] {
			b++
		}
		s.bandOf[k] = b
		if b+1 > s.nb {
			s.nb = b + 1
		}
	}
	s.inMic = make([]float64, N)
	s.inRef = make([]float64, N)
	s.olap = make([]float64, N)
	s.M = make([]complex128, N)
	s.R = make([]complex128, N)
	s.mb = make([]float64, s.nb)
	s.rb = make([]float64, s.nb)
	s.S = make([]float64, s.nb)
	s.hist = make([][]float64, supHist)
	for i := range s.hist {
		s.hist[i] = make([]float64, s.nb)
	}
	s.nf = make([]float64, s.nb)
	s.G = make([]float64, s.nb)
	for i := range s.G {
		s.G[i] = 1
	}
	s.gbin = make([]float64, nbins)
	s.out = make([]float32, H)
	s.cal = Calib{Rate: rate, W: make([]float64, s.nb)}
	s.calDone = make(chan Calib, 1)
	return s
}

// SetCalib задаёт готовую модель (например, загруженную из файла).
func (s *Suppressor) SetCalib(c Calib) bool {
	if c.Rate != s.Rate || len(c.W) != s.nb {
		return false
	}
	s.mu.Lock()
	if len(c.W0) != s.nb {
		c.W0 = append([]float64(nil), c.W...) // старый файл: что есть, то и считаем калибровкой
	}
	c.W = append([]float64(nil), c.W...)
	for i := range c.W { // загруженная модель тоже в пределах от калибровки
		c.W[i] = math.Max(0.7*c.W0[i], math.Min(2.5*c.W0[i], c.W[i]))
	}
	s.cal = c
	s.w0 = append([]float64(nil), c.W0...)
	s.d0 = c.D
	s.mu.Unlock()
	return true
}

func (s *Suppressor) Calib() Calib {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cal
	c.W = append([]float64(nil), s.cal.W...)
	c.W0 = append([]float64(nil), s.w0...)
	return c
}

// StartCalib: следующие frames шагов собирать данные калибровки (микрофон на выходе в это время заглушён).

func (s *Suppressor) StartCalib(frames int) {
	s.mu.Lock()
	s.calReq = frames
	s.mu.Unlock()
}

func (s *Suppressor) CalDone() <-chan Calib { return s.calDone }

// Process обрабатывает шаг из H отсчётов: ref — звук наушников (моно), mic — микрофон.
func (s *Suppressor) Process(ref, mic, out []float32) {
	N, H := s.N, s.H
	copy(s.inMic, s.inMic[H:])
	copy(s.inRef, s.inRef[H:])
	var me, re float64
	for i := 0; i < H; i++ {
		s.inMic[N-H+i] = float64(mic[i])
		s.inRef[N-H+i] = float64(ref[i])
		me += float64(mic[i]) * float64(mic[i])
		re += float64(ref[i]) * float64(ref[i])
	}
	me /= float64(H)
	re /= float64(H)
	for i := 0; i < N; i++ {
		s.M[i] = complex(s.inMic[i]*s.win[i], 0)
		s.R[i] = complex(s.inRef[i]*s.win[i], 0)
	}
	s.f.forward(s.M)
	s.f.forward(s.R)
	for b := range s.mb {
		s.mb[b], s.rb[b] = 0, 0
	}
	for k := 0; k <= N/2; k++ {
		b := s.bandOf[k]
		m, r := s.M[k], s.R[k]
		s.mb[b] += real(m)*real(m) + imag(m)*imag(m)
		s.rb[b] += real(r)*real(r) + imag(r)*imag(r)
	}
	// сглаживание опоры: мгновенная атака, спад ~25 мс
	for b := range s.S {
		if s.rb[b] > s.S[b] {
			s.S[b] = s.rb[b]
		} else {
			s.S[b] = 0.8*s.S[b] + 0.2*s.rb[b]
		}
	}
	s.hpos = (s.hpos + 1) % supHist
	copy(s.hist[s.hpos], s.S)

	// шумовой пол микрофона (медленно растёт, быстро падает)
	for b := range s.nf {
		if !s.nfInit || s.mb[b] < s.nf[b] {
			if !s.nfInit {
				s.nf[b] = s.mb[b]
			} else {
				s.nf[b] = 0.9*s.nf[b] + 0.1*s.mb[b]
			}
		} else {
			s.nf[b] *= 1.002
		}
	}
	s.nfInit = true

	s.mu.Lock()
	if s.calReq > 0 {
		s.calLeft = s.calReq
		s.calReq = 0
		s.calM = s.calM[:0]
		s.calS = s.calS[:0]
	}
	collecting := s.calLeft > 0
	cal := s.cal
	s.mu.Unlock()

	if collecting {
		s.calM = append(s.calM, append([]float64(nil), s.mb...))
		s.calS = append(s.calS, append([]float64(nil), s.S...))
		s.calLeft--
		if s.calLeft == 0 {
			c := s.solveCalib()
			c.W0 = append([]float64(nil), c.W...)
			s.mu.Lock()
			s.cal = c
			s.w0 = append([]float64(nil), c.W...)
			s.d0 = c.D
			s.mu.Unlock()
			select {
			case s.calDone <- c:
			default:
			}
		}
	}

	{
		var sm, sp float64
		for b := 2; b <= 18 && b < s.nb; b++ {
			p := s.nf[b] * vadNfMul
			if cal.Found && !collecting {
				p += cal.W[b] * s.hist[(s.hpos-cal.D+supHist)%supHist][b]
			}
			sm += s.mb[b]
			sp += p
		}
		ratio := sm / (sp + 1e-20)

		lv := 10 * math.Log10(me+1e-14)
		switch {
		case !s.bbInit:
			s.bbFloor, s.bbInit = lv, true
		case lv < s.bbFloor:
			s.bbFloor += 0.05 * (lv - s.bbFloor)
		case lv < s.bbFloor+9:
			s.bbFloor += 0.004 * (lv - s.bbFloor)
		default:
			s.bbFloor += 0.0015
		}
		raw := 0.0
		switch {
		case ratio > vadFull:
			raw = 1
		case ratio > vadStart:
			raw = (ratio - vadStart) / (vadFull - vadStart)
		}
		if lv < s.bbFloor+9 {
			raw = 0
		} else if lv < s.bbFloor+13 {
			raw *= (lv - s.bbFloor - 9) / 4
		}
		if raw > s.vad {
			s.vad += 0.5 * (raw - s.vad) // быстро включается
		} else {
			s.vad *= 0.985 // медленно отпускает (~0.4 с)
		}
		s.vadRaw = raw
	}
	voice := s.vad
	betaEff := s.Beta() * (1 - 0.25*voice) // пока говоришь ты — подавляем мягче

	// оценка наводки и усиления
	gmin := math.Pow(10, s.FloorDB/10) // в мощности
	if cal.Found && !collecting {
		lag := s.hist[(s.hpos-cal.D+supHist)%supHist]
		learn := voice < 0.1 && re > 1e-9
		for b := 0; b < s.nb; b++ {
			leak := cal.W[b] * lag[b]
			g := 1 - betaEff*leak/(s.mb[b]+1e-20)
			if g < gmin {
				g = gmin
			}
			if g > 1 {
				g = 1
			}
			if g < s.G[b] {
				s.G[b] = g
			} else {
				s.G[b] = 0.75*s.G[b] + 0.25*g
			}

			if s.Adapt && learn && lag[b] > 0 && b < len(s.w0) {
				pred := leak + s.nf[b]
				if leak > 0.5*s.nf[b] && s.mb[b] < 4*pred {
					target := (s.mb[b] - s.nf[b]) / lag[b]
					if target < 0 {
						target = 0
					}
					s.mu.Lock()

					rate := 0.006
					if target < s.cal.W[b] {
						rate = 0.0015
					}
					w := s.cal.W[b] + rate*(target-s.cal.W[b])
					if hi := 2.5 * s.w0[b]; w > hi {
						w = hi
					}
					if lo := 0.7 * s.w0[b]; w < lo {
						w = lo
					}
					s.cal.W[b] = w
					s.adapted = true
					s.mu.Unlock()
				}
			}
		}
	} else {
		for b := range s.G {
			s.G[b] = 1
		}
	}

	// усиление по бинам: сглаживание между соседними полосами
	for k := 0; k <= N/2; k++ {
		b := s.bandOf[k]
		g := s.G[b]
		if b > 0 {
			g = 0.25*s.G[b-1] + 0.5*g
		} else {
			g = 0.75 * g
		}
		if b+1 < s.nb {
			g += 0.25 * s.G[b+1]
		} else {
			g += 0.25 * s.G[b]
		}
		s.gbin[k] = math.Sqrt(g) // по амплитуде
	}
	for k := 0; k <= N/2; k++ {
		s.M[k] *= complex(s.gbin[k], 0)
		if k > 0 && k < N/2 {
			s.M[N-k] *= complex(s.gbin[k], 0)
		}
	}
	s.f.inverse(s.M)
	for i := 0; i < N; i++ {
		s.olap[i] += real(s.M[i]) * s.win[i]
	}
	var oe float64
	for i := 0; i < H; i++ {
		v := s.olap[i]
		if collecting {
			v = 0
		}
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		out[i] = float32(v)
		oe += v * v
	}
	oe /= float64(H)
	copy(s.olap, s.olap[H:])
	for i := N - H; i < N; i++ {
		s.olap[i] = 0
	}

	const a = 0.9
	s.sMic = a*s.sMic + (1-a)*me
	s.sRef = a*s.sRef + (1-a)*re
	s.sOut = a*s.sOut + (1-a)*oe
	if re > 1e-9 && !collecting {
		s.sDa = 0.97*s.sDa + 0.03*me
		s.sOa = 0.97*s.sOa + 0.03*oe
	}
	s.mu.Lock()
	s.pub = pubStats{s.sMic, s.sRef, s.sOut, s.vad, s.sDa, s.sOa}
	s.mu.Unlock()
	s.frames++
}

// Adapted сообщает, менялась ли модель по ходу работы (и сбрасывает признак).
func (s *Suppressor) Adapted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.adapted
	s.adapted = false
	return a
}

// Voice — ты сейчас говоришь (по мнению подавителя).
func (s *Suppressor) Voice() bool { return s.vad > 0.5 }

// VoiceLevel — сглаженная уверенность «ты говоришь», 0..1.
func (s *Suppressor) VoiceLevel() float64 { return s.vad }

func pctl(x []float64, p float64) float64 {
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	if len(y) == 0 {
		return 0
	}
	i := int(p * float64(len(y)-1))
	return y[i]
}

func corrF(x, y []float64) float64 {
	n := float64(len(x))
	if n == 0 {
		return 0
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
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

// solveCalib находит задержку D и коэффициенты W по собранным кадрам.
func (s *Suppressor) solveCalib() Calib {
	T := len(s.calM)
	c := Calib{Rate: s.Rate, W: make([]float64, s.nb)}
	if T < 2*supMaxD {
		return c
	}
	lg := func(v float64) float64 { return math.Log10(v + 1e-14) }

	bestD, bestC := 0, -1.0
	for d := 0; d < supMaxD; d++ {
		var sum float64
		var cnt int
		for b := 1; b < s.nb; b++ {
			x := make([]float64, 0, T)
			y := make([]float64, 0, T)
			for t := supMaxD; t < T; t++ {
				x = append(x, lg(s.calM[t][b]))
				y = append(y, lg(s.calS[t-d][b]))
			}
			sum += corrF(x, y)
			cnt++
		}
		if v := sum / float64(cnt); v > bestC {
			bestC, bestD = v, d
		}
	}
	c.D, c.Corr = bestD, bestC
	// шумовой пол и коэффициенты
	var leakSum, nfSum float64
	for b := 0; b < s.nb; b++ {
		mcol := make([]float64, 0, T)
		for t := supMaxD; t < T; t++ {
			mcol = append(mcol, s.calM[t][b])
		}
		nf := pctl(mcol, 0.15)
		var num, den float64
		for t := supMaxD; t < T; t++ {
			x := s.calS[t-bestD][b]
			y := s.calM[t][b] - nf
			if y < 0 {
				y = 0
			}
			num += x * y
			den += x * x
		}
		if den > 0 {
			c.W[b] = num / den
		}
		var pred float64
		for t := supMaxD; t < T; t++ {
			pred += c.W[b] * s.calS[t-bestD][b]
		}
		leakSum += pred
		nfSum += nf * float64(T-supMaxD)
	}
	c.LeakDB = 10 * math.Log10((leakSum+nfSum)/(nfSum+1e-20))
	c.Found = c.Corr > 0.3 && c.LeakDB > 1
	if !c.Found {
		for b := range c.W {
			c.W[b] = 0
		}
	}
	return c
}

func (s *Suppressor) Stats() SupStats {
	s.mu.Lock()
	cal := s.cal
	collecting := s.calLeft > 0 || s.calReq > 0
	p := s.pub
	s.mu.Unlock()
	st := SupStats{MicDB: db(p.mic), RefDB: db(p.ref), OutDB: db(p.out), Calibrating: collecting,
		Voice: p.vad > 0.5, Found: cal.Found, DelayMs: float64(cal.D*s.H) * 1000 / float64(s.Rate)}
	if p.oa > 0 {
		st.ReductionDB = db(p.da) - db(p.oa)
	}
	return st
}
