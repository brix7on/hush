package audio

import "math"

const (
	zeroCrossings = 12  // качество: ширина окна в нулях sinc
	tableRes      = 512 // точек таблицы ядра на единицу x
)

// Resampler — потоковый ресемплер на оконном sinc (Blackman).

type Resampler struct {
	inRate, outRate float64
	fc              float64 // отсечка (доля от Nyquist входа)
	half            float64 // полуширина ядра, в входных отсчётах
	table           []float64
	buf             []float32
	base            int64 // абсолютный индекс buf[0]
	pos             float64
	step, scale     float64
	Identity        bool
}

// NewResampler: forceActive=true отключает быстрый путь при равных частотах.

func NewResampler(inRate, outRate int, forceActive bool) *Resampler {
	r := &Resampler{inRate: float64(inRate), outRate: float64(outRate), scale: 1}
	r.step = r.inRate / r.outRate
	r.fc = math.Min(1, r.outRate/r.inRate)
	r.half = zeroCrossings / r.fc
	r.Identity = inRate == outRate && !forceActive
	n := int(r.half*tableRes) + 2
	r.table = make([]float64, n)
	for j := range r.table {
		x := float64(j) / tableRes
		r.table[j] = r.kernel(x)
	}
	return r
}

func (r *Resampler) kernel(x float64) float64 {
	if x >= r.half {
		return 0
	}
	a := r.fc * x
	var s float64 = 1
	if a != 0 {
		s = math.Sin(math.Pi*a) / (math.Pi * a)
	}
	t := x / r.half
	w := 0.42 + 0.5*math.Cos(math.Pi*t) + 0.08*math.Cos(2*math.Pi*t)
	return r.fc * s * w
}

func (r *Resampler) h(x float64) float64 {
	if x < 0 {
		x = -x
	}
	u := x * tableRes
	j := int(u)
	if j+1 >= len(r.table) {
		return 0
	}
	fr := u - float64(j)
	return r.table[j]*(1-fr) + r.table[j+1]*fr
}

// SetStepScale: scale>1 — выход «короче» (потребляем вход быстрее), <1 — длиннее.
func (r *Resampler) SetStepScale(s float64) { r.scale = s }

// Process добавляет вход и дописывает в out все готовые выходные отсчёты.
func (r *Resampler) Process(in, out []float32) []float32 {
	if r.Identity {
		return append(out, in...)
	}
	r.buf = append(r.buf, in...)
	for {
		hi := int64(math.Floor(r.pos + r.half))
		if hi >= r.base+int64(len(r.buf)) {
			break
		}
		lo := int64(math.Ceil(r.pos - r.half))
		var acc, wsum float64
		for i := lo; i <= hi; i++ {
			w := r.h(r.pos - float64(i))
			wsum += w
			if i >= r.base {
				acc += w * float64(r.buf[i-r.base])
			}
		}
		if wsum != 0 {
			acc /= wsum
		}
		out = append(out, float32(acc))
		r.pos += r.step * r.scale
	}
	// выбросить вход, который больше не понадобится
	keep := int64(math.Ceil(r.pos-r.half)) - 1
	if drop := keep - r.base; drop > 0 {
		if drop > int64(len(r.buf)) {
			drop = int64(len(r.buf))
		}
		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.base += drop
	}
	return out
}
