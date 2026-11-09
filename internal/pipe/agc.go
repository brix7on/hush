package pipe

import (
	"math"
	"sync/atomic"
)

// AGC — авто-громкость: подтягивает тихую речь и приглушает слишком громкую до целевого уровня.

type AGC struct {
	enabled  atomic.Bool
	TargetDB float64 // целевой уровень речи (RMS), дБFS
	MaxUpDB  float64
	MaxDnDB  float64

	gain    float64 // текущее усиление, дБ
	pubGain atomic.Uint64
	floor   float64 // оценка уровня фона, дБ (мощность блока)
	hold    int
	started bool
}

func NewAGC() *AGC {
	return &AGC{TargetDB: -23, MaxUpDB: 18, MaxDnDB: -9, floor: -70}
}

func (a *AGC) SetEnabled(on bool) { a.enabled.Store(on) }
func (a *AGC) Enabled() bool      { return a.enabled.Load() }

// GainDB — текущее усиление AGC (для индикатора).
func (a *AGC) GainDB() float64 { return math.Float64frombits(a.pubGain.Load()) }

func (a *AGC) Process(x []float32) {
	var sum float64
	for _, v := range x {
		sum += float64(v) * float64(v)
	}
	lvl := 10 * math.Log10(sum/float64(len(x))+1e-12) // дБ, мощность блока
	// фон: быстро падает к тишине, медленно растёт
	if lvl < a.floor {
		a.floor = 0.7*a.floor + 0.3*lvl
	} else {
		a.floor += 0.0008 * (lvl - a.floor)
	}
	prev := a.gain
	if !a.enabled.Load() {
		// плавно уводим в 0, чтобы выключение не щёлкало
		a.gain *= 0.97
		if math.Abs(a.gain) < 0.01 {
			a.gain = 0
		}
	} else {
		speech := lvl > a.floor+9 && lvl > -52
		if speech {
			a.hold = 40 // ~0.2 с «хвоста» речи
		} else if a.hold > 0 {
			a.hold--
		}
		if speech {
			want := a.TargetDB - lvl
			if want > a.MaxUpDB {
				want = a.MaxUpDB
			}
			if want < a.MaxDnDB {
				want = a.MaxDnDB
			}
			if want < a.gain {
				a.gain += 0.25 * (want - a.gain) // снижаем быстро (~20 мс)
			} else {
				a.gain += 0.004 * (want - a.gain) // повышаем медленно (~1.5 с)
			}
		}
	}
	a.pubGain.Store(math.Float64bits(a.gain))
	if prev == 0 && a.gain == 0 {
		return
	}
	g0, g1 := math.Pow(10, prev/20), math.Pow(10, a.gain/20)
	n := float64(len(x))
	for i, v := range x {
		g := g0 + (g1-g0)*float64(i+1)/n
		x[i] = float32(float64(v) * g)
	}
}
