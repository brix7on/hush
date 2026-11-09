package pipe

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/brix7on/hush/internal/aec"
)

const (
	histLen      = 65536 // ~1.4 с выровненных данных для оценки задержки
	decim        = 4
	estEvery     = 562 // блоков по 256 ≈ 3 с
	lagMin       = Rate * 15 / 1000
	lagMax       = Rate * 100 / 1000
	lagTarget    = Rate * 40 / 1000
	searchMax    = Rate * 220 / 1000
	confMin      = 8.0
	needConsist  = 2
	activeThresh = 1e-9
)

// Engine прогоняет микрофон и опору через фильтр блок за блоком.
type Engine struct {
	Mic, Ref *StreamIn
	F        *aec.Filter
	S        *aec.Suppressor
	smono    []float32
	Bypass   bool
	Amp      *Amp          // усиление на выходе (может быть nil)
	Dn       *aec.Denoiser // шумодав (может быть nil)
	EQ       *EQ           // эквалайзер (может быть nil)
	Agc      *AGC          // авто-громкость (может быть nil)
	Open     *atomic.Bool  // микрофон по кнопке: nil или true — открыт
	gate     float64
	NoAuto   bool // не подстраивать задержку автоматически
	OutBits  atomic.Uint64
	Hold     int64

	RefStale int64

	B       int
	next    int64
	started bool
	rbs     [][]float32
	mb      []float32
	ob      []float32

	First          int64 // индекс шкалы, с которого начинается выход
	Blocks, Waited int
	LateRef        int
	LateMaxMs      float64 // самое долгое ожидание опоры, мс
	waitFrom       int64   // с какого «сейчас» начали ждать опору (0 — не ждём)

	// оценка реальной задержки эха
	hr, hm       []float32
	hpos, hcount int
	sinceEst     int
	activeSince  int
	mu           sync.Mutex
	busy         bool
	fresh        bool
	estLag       int
	estConf      float64
	estValid     bool
	cand         int
	candLag      int
	Corrections  int
}

// NewEngineS — движок с подавителем наводки по огибающей (опора — моно-сумма каналов).
func NewEngineS(mic, ref *StreamIn, sp *aec.Suppressor, refs int) *Engine {
	e := &Engine{gate: 1, Mic: mic, Ref: ref, S: sp, B: sp.H, Hold: Rate * 31 / 1000, RefStale: Rate * 35 / 1000, NoAuto: true,
		mb: make([]float32, sp.H), ob: make([]float32, sp.H), smono: make([]float32, sp.H),
		hr: make([]float32, histLen), hm: make([]float32, histLen)}
	if refs < 1 {
		refs = 1
	}
	e.rbs = make([][]float32, refs)
	for k := range e.rbs {
		e.rbs[k] = make([]float32, sp.H)
	}
	return e
}

func NewEngine(mic, ref *StreamIn, f *aec.Filter) *Engine {
	e := &Engine{gate: 1, Mic: mic, Ref: ref, F: f, B: f.B, Hold: Rate * 31 / 1000, RefStale: Rate * 35 / 1000,
		mb: make([]float32, f.B), ob: make([]float32, f.B),
		hr: make([]float32, histLen), hm: make([]float32, histLen)}
	e.rbs = make([][]float32, f.K)
	for k := range e.rbs {
		e.rbs[k] = make([]float32, f.B)
	}
	return e
}

// Estimate возвращает последнюю оценку: на сколько отсчётов эхо в микрофоне позже опоры на шкале, и уверенность.

func (e *Engine) Estimate() (lagMs float64, conf float64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return float64(e.estLag) * 1000 / Rate, e.estConf, e.estValid
}

func (e *Engine) record() {
	var act float64
	for i := 0; i < e.B; i++ {
		var r float32
		for k := range e.rbs {
			r += e.rbs[k][i]
		}
		e.hr[e.hpos] = r
		e.hm[e.hpos] = e.mb[i]
		e.hpos = (e.hpos + 1) % histLen
		act += float64(r) * float64(r)
	}
	if e.hcount < histLen {
		e.hcount += e.B
	}
	e.sinceEst++
	if act/float64(e.B) > activeThresh {
		e.activeSince++
	}
	if e.sinceEst >= estEvery && e.F != nil {
		start := e.activeSince*2 > e.sinceEst && e.hcount >= histLen
		e.sinceEst, e.activeSince = 0, 0
		e.mu.Lock()
		busy := e.busy
		e.mu.Unlock()
		if start && !busy {
			r := make([]float32, histLen/decim)
			m := make([]float32, histLen/decim)
			for i := range r {
				var sr, sm float32
				for j := 0; j < decim; j++ {
					idx := (e.hpos + i*decim + j) % histLen
					sr += e.hr[idx]
					sm += e.hm[idx]
				}
				r[i], m[i] = sr/decim, sm/decim
			}
			e.mu.Lock()
			e.busy = true
			e.mu.Unlock()
			go func() {
				lag, conf := aec.EstimateDelay(r, m, searchMax/decim)
				e.mu.Lock()
				e.estLag, e.estConf, e.estValid = lag*decim, conf, conf >= confMin
				e.busy, e.fresh = false, true
				e.mu.Unlock()
			}()
		}
	}
}

// applyEstimate сдвигает опору, если эхо стабильно вне окна фильтра.

func (e *Engine) applyEstimate() {
	e.mu.Lock()
	if !e.fresh {
		e.mu.Unlock()
		return
	}
	e.fresh = false
	lag, valid := e.estLag, e.estValid
	e.mu.Unlock()
	if e.NoAuto || e.F == nil {
		return
	}
	if valid && (lag < lagMin || lag > lagMax) {
		if e.cand > 0 && math.Abs(float64(lag-e.candLag)) < float64(Rate/50) {
			e.cand++
		} else {
			e.cand = 1
		}
		e.candLag = lag
		if e.cand >= needConsist {
			e.Ref.SetDelay(e.Ref.CurDelay() + int64(lag-lagTarget))
			e.F.Reset()
			e.Corrections++
			e.cand = 0
		}
	} else {
		e.cand = 0
	}
}

// Step обрабатывает все готовые блоки; nowIdx — «сейчас» на шкале (по QPC).

func (e *Engine) Step(nowIdx int64, out []float32) []float32 {
	e.applyEstimate()
	B := int64(e.B)
	for {
		micEnd, ok := e.Mic.End()
		if !ok {
			return out
		}
		if !e.started {
			// начинаем с границы блока, чтобы шкала шла ровными блоками
			e.next = micEnd - (micEnd % B)
			if e.next < micEnd {
				e.next += B
			}
			e.First = e.next
			e.started = true
		}
		s := e.next
		if micEnd < s+B {
			return out
		}
		refEnd, rok := e.Ref.End()
		if !rok || refEnd < s+B {
			alive := e.RefStale <= 0 || (rok && nowIdx-(refEnd-e.Ref.CurDelay()) <= e.RefStale)
			if alive && nowIdx < s+B+e.Hold {
				e.Waited++
				if e.waitFrom == 0 {
					e.waitFrom = nowIdx
				}
				return out // ждём опору: возможно, пакет чуть опаздывает
			}
		}
		if e.waitFrom != 0 {
			// микрофон был готов, а выход стоял — ждали звук наушников
			if w := nowIdx - e.waitFrom; w > Rate*4/1000 {
				e.LateRef++
				if ms := float64(w) * 1000 / Rate; ms > e.LateMaxMs {
					e.LateMaxMs = ms
				}
			}
			e.waitFrom = 0
		}
		e.Mic.ReadAt(0, s, e.mb)
		for k := range e.rbs {
			e.Ref.ReadAt(k, s, e.rbs[k])
		}
		switch {
		case e.Bypass:
			copy(e.ob, e.mb)
		case e.S != nil:
			inv := 1 / float32(len(e.rbs))
			for i := range e.smono {
				var v float32
				for k := range e.rbs {
					v += e.rbs[k][i]
				}
				e.smono[i] = v * inv
			}
			e.S.Process(e.smono, e.mb, e.ob)
		default:
			e.F.Process(e.rbs, e.mb, e.ob)
		}
		if !e.Bypass {
			if e.Dn != nil {
				if e.S != nil {
					e.Dn.VoiceHint(e.S.VoiceLevel())
				}
				e.Dn.Process(e.ob)
			}
			if e.EQ != nil {
				e.EQ.Process(e.ob)
			}
			if e.Agc != nil {
				e.Agc.Process(e.ob)
			}
		}
		if e.Amp != nil {
			e.Amp.Process(e.ob)
		}
		e.applyGate()
		e.trackOut()
		e.record()
		out = append(out, e.ob...)
		e.next += B
		e.Blocks++
	}
}

// Next — индекс шкалы, до которого микрофон уже обработан (ok=false — ещё не начали).
func (e *Engine) Next() (int64, bool) { return e.next, e.started }

// AlgoDelay — собственная задержка обработки, отсчётов 48 кГц.

func (e *Engine) AlgoDelay() int64 {
	if e.Bypass {
		return 0
	}
	var d int64
	if e.S != nil {
		d += int64(e.S.N - e.S.H)
	}
	if e.Dn != nil {
		d += int64(e.Dn.Delay())
	}
	return d
}

func (e *Engine) trackOut() {
	var sum float64
	for _, v := range e.ob {
		sum += float64(v) * float64(v)
	}
	db := 10 * math.Log10(sum/float64(len(e.ob))+1e-12)
	cur := math.Float64frombits(e.OutBits.Load())
	if cur == 0 {
		cur = -120
	}
	if db > cur {
		cur = db
	} else {
		cur = 0.92*cur + 0.08*db
	}
	e.OutBits.Store(math.Float64bits(cur))
}

// OutDB — уровень выхода, дБFS.
func (e *Engine) OutDB() float64 {
	v := math.Float64frombits(e.OutBits.Load())
	if v == 0 {
		return -120
	}
	return v
}

// applyGate плавно (за ~10 мс) открывает/закрывает микрофон по кнопке.
func (e *Engine) applyGate() {
	target := 1.0
	if e.Open != nil && !e.Open.Load() {
		target = 0
	}
	if e.gate == target && target == 1 {
		return
	}
	const step = 1.0 / 480
	for i, v := range e.ob {
		if e.gate < target {
			e.gate = math.Min(target, e.gate+step)
		} else if e.gate > target {
			e.gate = math.Max(target, e.gate-step)
		}
		e.ob[i] = float32(float64(v) * e.gate)
	}
}
