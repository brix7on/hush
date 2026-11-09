package pipe

import (
	"fmt"
	"math"
)

func rmsDB(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return 10 * math.Log10(s/float64(len(x))+1e-12)
}

// Extract вырезает общий отрезок двух потоков: [0] — микрофон, дальше каналы наушников.

func Extract(mic, ref *StreamIn, now int64) (chans [][]float32, notes []string, err error) {
	mFirst, mok := mic.First()
	rFirst, rok := ref.First()
	if !mok {
		return nil, nil, fmt.Errorf("с микрофона не пришло ни одного пакета")
	}
	if !rok {
		return nil, nil, fmt.Errorf("из наушников не пришло звука — включи музыку или видео и запусти запись ещё раз")
	}
	ref.PadTo(now)
	mic.PadTo(now)
	a := mFirst
	if rFirst > a {
		a = rFirst
	}
	a += Rate
	mEnd, _ := mic.End()
	rEnd, _ := ref.End()
	b := mEnd
	if rEnd < b {
		b = rEnd
	}
	b -= Rate / 5
	n := int(b - a)
	if n < Rate*3 {
		return nil, nil, fmt.Errorf("получилось слишком мало данных (%d отсчётов)", n)
	}
	m := make([]float32, n)
	mic.ReadAt(0, a, m)
	chans = [][]float32{m}
	notes = append(notes, fmt.Sprintf("канал 0 = микрофон (%.1f дБ)", rmsDB(m)))
	for c := 0; c < ref.Channels(); c++ {
		x := make([]float32, n)
		ref.ReadAt(c, a, x)
		var mx float32
		for _, v := range x {
			if v < 0 {
				v = -v
			}
			if v > mx {
				mx = v
			}
		}
		if mx < 1e-6 {
			notes = append(notes, fmt.Sprintf("наушники, исходный канал %d: тишина (не записан)", c))
			continue
		}
		chans = append(chans, x)
		notes = append(notes, fmt.Sprintf("канал %d = наушники, исходный канал %d (%.1f дБ)", len(chans)-1, c, rmsDB(x)))
	}
	notes = append(notes, fmt.Sprintf("длина %.1f с, синхронизаций: микрофон %d, наушники %d",
		float64(n)/Rate, mic.ResyncCount(), ref.ResyncCount()))
	return chans, notes, nil
}
