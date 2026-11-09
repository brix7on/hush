// Package pipe — выравнивание потоков микрофона и «наушников» по времени,
// запуск фильтра и подготовка выхода. Не зависит от Windows, поэтому тестируется на Linux.
package pipe

import "sync"

const Rate = 48000 // внутренняя частота

// Timeline — кольцевой буфер моно-сигнала с абсолютной нумерацией отсчётов.

type Timeline struct {
	mu    sync.Mutex
	buf   []float32
	size  int64
	end   int64 // следующий индекс для записи
	first int64
	set   bool
}

func NewTimeline(size int) *Timeline {
	return &Timeline{buf: make([]float32, size), size: int64(size)}
}

// First — индекс первого записанного отсчёта.
func (t *Timeline) First() (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.first, t.set
}

func (t *Timeline) End() (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.end, t.set
}

// Start задаёт индекс первого отсчёта (один раз).
func (t *Timeline) Start(idx int64) {
	t.mu.Lock()
	if !t.set {
		t.end, t.first, t.set = idx, idx, true
	}
	t.mu.Unlock()
}

func (t *Timeline) Append(s []float32) {
	t.mu.Lock()
	for _, v := range s {
		t.buf[((t.end%t.size)+t.size)%t.size] = v
		t.end++
	}
	t.mu.Unlock()
}

// PadTo дописывает нули до индекса idx.
func (t *Timeline) PadTo(idx int64) {
	t.mu.Lock()
	if idx-t.end > t.size {
		t.end = idx - t.size
	}
	for t.end < idx {
		t.buf[((t.end%t.size)+t.size)%t.size] = 0
		t.end++
	}
	t.mu.Unlock()
}

// Shift сдвигает конец записи на d отсчётов (d>0 — вставить тишину, d<0 — отступить назад).
func (t *Timeline) Shift(d int64) {
	t.mu.Lock()
	if d > 0 {
		if d > t.size {
			d = t.size
		}
		for i := int64(0); i < d; i++ {
			t.buf[((t.end%t.size)+t.size)%t.size] = 0
			t.end++
		}
	} else {
		t.end += d
	}
	t.mu.Unlock()
}

// ReadAt копирует [start, start+len(dst)) в dst; чего нет в буфере — нули.
func (t *Timeline) ReadAt(start int64, dst []float32) {
	t.mu.Lock()
	for i := range dst {
		idx := start + int64(i)
		if idx >= t.end || idx < t.end-t.size {
			dst[i] = 0
		} else {
			dst[i] = t.buf[((idx%t.size)+t.size)%t.size]
		}
	}
	t.mu.Unlock()
}

// StreamIn принимает пакеты одного устройства (моно или несколько каналов, уже 48 кГц) и кладёт их на общую.

type StreamIn struct {
	T0    int64 // QPC (100 нс), общее начало шкалы
	Delay int64 // на сколько отсчётов опоздать потоку на шкале
	Tol   int64

	size    int
	mu      sync.Mutex
	tl      []*Timeline
	resyncs int
}

func NewStreamIn(size int, t0 int64, delay int64) *StreamIn {
	return &StreamIn{size: size, T0: t0, Delay: delay, Tol: Rate * 40 / 1000}
}

func (s *StreamIn) timelines() []*Timeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tl
}

// Channels — сколько каналов уже приходило (0 — пока ничего).
func (s *StreamIn) Channels() int { return len(s.timelines()) }

// End — конец записи (по первому каналу).
func (s *StreamIn) End() (int64, bool) {
	tl := s.timelines()
	if len(tl) == 0 {
		return 0, false
	}
	return tl[0].End()
}

func (s *StreamIn) First() (int64, bool) {
	tl := s.timelines()
	if len(tl) == 0 {
		return 0, false
	}
	return tl[0].First()
}

// ReadAt читает канал ch; отсутствующий канал или данные — нули.
func (s *StreamIn) ReadAt(ch int, start int64, dst []float32) {
	tl := s.timelines()
	if ch >= len(tl) {
		for i := range dst {
			dst[i] = 0
		}
		return
	}
	tl[ch].ReadAt(start, dst)
}

// PadTo дописывает нули до индекса idx во всех каналах.
func (s *StreamIn) PadTo(idx int64) {
	for _, t := range s.timelines() {
		t.PadTo(idx)
	}
}

func (s *StreamIn) SetDelay(d int64) {
	s.mu.Lock()
	old := s.Delay
	s.Delay = d
	tl := s.tl
	s.mu.Unlock()
	for _, t := range tl {
		t.Shift(d - old)
	}
}

func (s *StreamIn) CurDelay() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Delay
}

// IdxFromQPC переводит метку QPC (100 нс) в индекс шкалы.
func IdxFromQPC(qpc, t0 int64) int64 {
	d := qpc - t0
	return d/10000000*Rate + (d%10000000)*Rate/10000000
}

func (s *StreamIn) ensure(nch int) []*Timeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.tl) < nch {
		s.tl = append(s.tl, NewTimeline(s.size))
	}
	return s.tl
}

// Push: pkt[c] — отсчёты канала c (48 кГц), все каналы одной длины; qpc — метка начала пакета (0 — неизвестна).

func (s *StreamIn) Push(pkt [][]float32, qpc int64) {
	if len(pkt) == 0 || len(pkt[0]) == 0 {
		return
	}
	tl := s.ensure(len(pkt))
	delay := s.CurDelay()
	end, set := tl[0].End()
	if qpc != 0 {
		pi := IdxFromQPC(qpc, s.T0) + delay
		if !set {
			for _, t := range tl {
				t.Start(pi)
			}
		} else if diff := pi - end; diff > s.Tol {
			for _, t := range tl {
				t.PadTo(pi) // в потоке была пауза (например, в наушниках тишина)
			}
			s.bump()
		} else if diff < -s.Tol {
			drop := -diff
			if drop >= int64(len(pkt[0])) {
				return
			}
			cut := make([][]float32, len(pkt))
			for c := range pkt {
				cut[c] = pkt[c][drop:]
			}
			pkt = cut
			s.bump()
		}
	} else if !set {
		for _, t := range tl {
			t.Start(0)
		}
	}
	for c := range pkt {
		tl[c].Append(pkt[c])
	}
}

func (s *StreamIn) bump() {
	s.mu.Lock()
	s.resyncs++
	s.mu.Unlock()
}

func (s *StreamIn) ResyncCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resyncs
}
