package pipe

import (
	"math"
	"time"

	"github.com/brix7on/hush/internal/audio"
)

// Sink переводит поток 48 кГц в частоту устройства вывода и держит запас буфера.

type Sink struct {
	rs     *audio.Resampler
	q      []float32
	rate   int
	Target int // желаемая заполненность устройства, кадров
	Base   int
	Drops  int

	QuietSlack int
	QuietDrops int // сколько кадров выкинуто в тишине

	floor  float64
	subMin [8]float64

	out []float32 // буфер результата Take

	fillCur, fillPrev, fillN int // минимум заполненности за текущие и прошлые ~0.1 с
	fillReady                bool
	subIdx                   int
	subCnt                   int

	// подстройка цели под провалы
	starveT time.Duration
	calmT   time.Duration
	started bool
	Starves int // сколько всего было провалов (для статистики)
}

// quietLevel — ниже этого (≈ −60 дБFS) звук считается тишиной всегда.

const (
	quietLevel     = 1e-3
	quietOverFloor = 2.5
)

func NewSink(devRate int, targetMs int) *Sink {
	t := devRate * targetMs / 1000
	return &Sink{rs: audio.NewResampler(Rate, devRate, true), rate: devRate, Target: t, Base: t}
}

// SetBaseMs меняет нижнюю границу цели.
func (s *Sink) SetBaseMs(ms float64) {
	nb := int(float64(s.rate)*ms/1000 + 0.5)
	if nb < 1 || nb == s.Base {
		return
	}
	if s.Target <= s.Base || s.Target < nb {
		s.Target = nb
	}
	s.Base = nb
}

// Queued — сколько кадров ждёт в очереди (ещё не отдано устройству).
func (s *Sink) Queued() int { return len(s.q) }

// Adapt подстраивает цель по заполненности устройства.

func (s *Sink) Adapt(padding int, now time.Duration) {
	if !s.started {
		s.started = true
		s.starveT, s.calmT = now-time.Hour, now
	}
	if padding == 0 && now > 2*time.Second && now-s.starveT > 300*time.Millisecond {
		s.Starves++
		s.starveT = now
		if s.Target < s.rate*60/1000 {
			s.Target += s.rate * 3 / 1000
		}
		s.calmT = now
	} else if s.Target > s.Base && now-s.starveT > time.Minute && now-s.calmT > time.Minute {
		s.Target -= s.rate / 1000
		if s.Target < s.Base {
			s.Target = s.Base
		}
		s.calmT = now
	}
}

func (s *Sink) Feed(mono48 []float32) {

	const win = 240
	for i := 0; i+win <= len(mono48); i += win {
		var e float64
		for _, v := range mono48[i : i+win] {
			e += float64(v) * float64(v)
		}
		r := math.Sqrt(e / win)
		if r < 1e-5 {
			continue
		}
		if s.subMin[s.subIdx] == 0 || r < s.subMin[s.subIdx] {
			s.subMin[s.subIdx] = r
		}
		s.subCnt++
		if s.subCnt >= 40 { // 40 × 5 мс = 0.2 с
			s.subCnt = 0
			s.subIdx = (s.subIdx + 1) % len(s.subMin)
			s.subMin[s.subIdx] = 0
		}
		s.floor = 0
		for _, m := range s.subMin {
			if m > 0 && (s.floor == 0 || m < s.floor) {
				s.floor = m
			}
		}
	}
	s.q = s.rs.Process(mono48, s.q)
}

func (s *Sink) dropQuiet(want int) int {
	fade := s.rate / 500 // 2 мс
	minDrop := s.rate / 1000
	if want < minDrop {
		return 0
	}
	if want > s.rate*10/1000 { // за раз не больше 10 мс
		want = s.rate * 10 / 1000
	}
	if len(s.q) < want+fade {
		want = len(s.q) - fade
		if want < minDrop {
			return 0
		}
	}
	thr := math.Max(quietLevel, s.floor*quietOverFloor)
	var e float64
	for _, v := range s.q[:want+fade] {
		e += float64(v) * float64(v)
	}
	if math.Sqrt(e/float64(want+fade)) > thr {
		return 0
	}
	for i := 0; i < fade; i++ {
		w := float32(i+1) / float32(fade+1)
		s.q[want+i] = s.q[i]*(1-w) + s.q[want+i]*w
	}
	s.q = s.q[want:]
	return want
}

// Take: padding — сколько кадров уже стоит в буфере устройства, free — сколько ещё влезет.

func (s *Sink) Take(padding, free int) []float32 {
	fill := padding + len(s.q)
	if s.fillN == 0 || fill < s.fillCur {
		s.fillCur = fill
	}
	s.fillN++
	if s.fillN >= 100 { // ~0.1 с при опросе раз в 1 мс
		s.fillPrev, s.fillN = s.fillCur, 0
		s.fillReady = true
	}
	minFill := s.fillCur
	if s.fillReady && s.fillPrev < minFill {
		minFill = s.fillPrev
	}
	excess := minFill - s.Target

	if s.QuietSlack > 0 && s.fillReady && excess > s.QuietSlack {
		if n := s.dropQuiet(excess - s.rate/1000); n > 0 {
			s.QuietDrops += n
			fill -= n
			s.fillCur -= n
			s.fillPrev -= n
			excess -= n
		}
	}
	if s.fillReady {
		err := float64(excess) / float64(s.Target)
		if err > 1 {
			err = 1
		} else if err < -1 {
			err = -1
		}
		s.rs.SetStepScale(1 + 0.002*err) // ±0.2 % максимум
	}
	if s.fillReady && excess > s.rate*80/1000 {
		drop := excess - s.rate*10/1000
		if drop > len(s.q) {
			drop = len(s.q)
		}
		s.q = s.q[drop:]
		s.fillCur -= drop
		s.fillPrev -= drop
		s.Drops++
	}
	n := len(s.q)
	if n > free {
		n = free
	}

	if limit := s.Target + s.rate*3/1000 - padding; n > limit {
		n = limit
	}
	if n <= 0 {
		return nil
	}

	if cap(s.out) < n {
		s.out = make([]float32, n, n+s.rate/100)
	}
	out := s.out[:n]
	copy(out, s.q[:n])
	s.q = s.q[n:]
	return out
}

// MicBaseMs — сколько звука держать в запасе в самый «голодный» момент.

func MicBaseMs(periodMs float64) float64 {
	_ = periodMs
	return 5
}

// HoldFor — сколько движку можно ждать запоздавший звук наушников, отсчётов 48 кГц.

func (s *Sink) HoldFor(padding int, maxHold int64) int64 {
	fill := int64(padding+len(s.q)) * Rate / int64(s.rate)
	h := fill - Rate*2/1000
	if h < 0 {
		h = 0
	}
	if h > maxHold {
		h = maxHold
	}
	return h
}
