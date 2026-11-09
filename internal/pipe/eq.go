package pipe

import (
	"math"
	"sync/atomic"
)

type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

func (q *biquad) proc(x float64) float64 {
	y := q.b0*x + q.z1
	q.z1 = q.b1*x - q.a1*y + q.z2
	q.z2 = q.b2*x - q.a2*y
	return y
}

func hpf(f, qf float64) biquad {
	w := 2 * math.Pi * f / Rate
	al := math.Sin(w) / (2 * qf)
	c := math.Cos(w)
	a0 := 1 + al
	return biquad{b0: (1 + c) / 2 / a0, b1: -(1 + c) / a0, b2: (1 + c) / 2 / a0, a1: -2 * c / a0, a2: (1 - al) / a0}
}

func peak(f, qf, gainDB float64) biquad {
	A := math.Pow(10, gainDB/40)
	w := 2 * math.Pi * f / Rate
	al := math.Sin(w) / (2 * qf)
	c := math.Cos(w)
	a0 := 1 + al/A
	return biquad{b0: (1 + al*A) / a0, b1: -2 * c / a0, b2: (1 - al*A) / a0, a1: -2 * c / a0, a2: (1 - al/A) / a0}
}

func highShelf(f, gainDB float64) biquad {
	A := math.Pow(10, gainDB/40)
	w := 2 * math.Pi * f / Rate
	c, s := math.Cos(w), math.Sin(w)
	al := s / 2 * math.Sqrt(2) // S=1
	sq := 2 * math.Sqrt(A) * al
	a0 := (A + 1) - (A-1)*c + sq
	return biquad{
		b0: A * ((A + 1) + (A-1)*c + sq) / a0,
		b1: -2 * A * ((A - 1) + (A+1)*c) / a0,
		b2: A * ((A + 1) + (A-1)*c - sq) / a0,
		a1: 2 * ((A - 1) - (A+1)*c) / a0,
		a2: ((A + 1) - (A-1)*c - sq) / a0,
	}
}

// Пресеты эквалайзера.
const (
	EQOff = iota
	EQClear
	EQSoft // «мягче»: срез НЧ, меньше шипения и свиста
	eqCount
)

// EQ — простой эквалайзер голоса из нескольких фильтров.
type EQ struct {
	preset atomic.Int32
	cur    int32
	chain  []biquad
}

func NewEQ() *EQ { return &EQ{} }

func (e *EQ) SetPreset(p int) {
	if p < 0 || p >= eqCount {
		p = 0
	}
	e.preset.Store(int32(p))
}
func (e *EQ) Preset() int { return int(e.preset.Load()) }

func (e *EQ) build(p int32) {
	switch p {
	case EQClear:
		e.chain = []biquad{hpf(90, 0.707), peak(250, 1.0, -2.5), peak(3200, 0.9, 3.0), highShelf(9000, -1.5)}
	case EQSoft:
		e.chain = []biquad{hpf(80, 0.707), peak(5500, 1.2, -2.5), highShelf(8000, -4)}
	default:
		e.chain = nil
	}
}

func (e *EQ) Process(x []float32) {
	p := e.preset.Load()
	if p != e.cur {
		e.cur = p
		e.build(p)
	}
	if len(e.chain) == 0 {
		return
	}
	for i, v := range x {
		y := float64(v)
		for k := range e.chain {
			y = e.chain[k].proc(y)
		}
		x[i] = float32(y)
	}
}
