package pipe

import (
	"math"
	"sync/atomic"
)

// Amp — усилитель с ограничителем: умножает сигнал на заданное усиление и не даёт пикам подняться выше Ceil.

type Amp struct {
	gainBits atomic.Uint64
	Ceil     float64
	lim      float64
	release  float64
	limBits  atomic.Uint64
}

func NewAmp(gainDB float64) *Amp {
	a := &Amp{Ceil: 0.89, lim: 1, release: math.Pow(10, 10.0/20/(0.3*Rate))}
	a.SetGainDB(gainDB)
	return a
}

func (a *Amp) SetGainDB(db float64) { a.gainBits.Store(math.Float64bits(db)) }
func (a *Amp) GainDB() float64      { return math.Float64frombits(a.gainBits.Load()) }

func (a *Amp) Process(x []float32) {
	db := a.GainDB()
	if db == 0 && a.lim == 1 {
		a.limBits.Store(0)
		return
	}
	g := math.Pow(10, db/20)
	minLim := 1.0
	for i, v := range x {
		y := float64(v) * g
		if p := math.Abs(y) * a.lim; p > a.Ceil {
			a.lim = a.Ceil / math.Abs(y)
		}
		y *= a.lim
		if a.lim < minLim {
			minLim = a.lim
		}
		a.lim *= a.release
		if a.lim > 1 {
			a.lim = 1
		}
		x[i] = float32(y)
	}
	a.limBits.Store(math.Float64bits(-20 * math.Log10(minLim)))
}

// Limiting — текущее ослабление ограничителем, дБ.
func (a *Amp) Limiting() float64 { return math.Float64frombits(a.limBits.Load()) }
