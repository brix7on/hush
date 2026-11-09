package pipe

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/brix7on/hush/internal/aec"
)

// Моделирование всей цепочки по времени: микрофон пачками по micP мс, опора (loopback)
// пачками по 10 мс, вывод опрашивается раз в ~1 мс с дрожанием таймера, устройство
// вывода съедает 48 отсчётов в мс (с дрейфом часов). Меряем реальную задержку:
// от момента записи отсчёта микрофоном до момента, когда он будет сыгран из кабеля.
type simCfg struct {
	micP      float64 // мс
	newLogic  bool    // новые правила (не ждать молчащую опору, выкидывать лишнее в тишине)
	refGap    [2]float64
	jitterMs  float64
	baseMs    float64
	secs      float64
	refDelayM float64
	noise     float64 // уровень фона в паузах (по умолчанию 2e-4 ≈ −74 дБFS)
	refStall  float64 // доля пакетов наушников, которые приходят с опозданием на 30 мс (Windows притормозила)
}

type simRes struct {
	avgMs, p95Ms float64
	starves      int
	targetMs     float64
	quietDropMs  float64
}

func simChain(c simCfg) simRes {
	rng := rand.New(rand.NewSource(1))
	t0 := int64(0)
	mic := NewStreamIn(Rate*2, t0, 0)
	ref := NewStreamIn(Rate*2, t0, int64(c.refDelayM*Rate/1000))
	sup := aec.NewSuppressor(Rate)
	eng := NewEngineS(mic, ref, sup, 2)
	if !c.newLogic {
		eng.RefStale = 0
	}
	sink := NewSink(48000, 12)
	if c.newLogic {
		sink.SetBaseMs(c.baseMs)
		sink.QuietSlack = 48000 * 4 / 1000
	}
	const drift = 150e-6
	qpcOf := func(ms float64) int64 { return int64((ms + 1) * 10000) } // 100 нс (0 — «нет метки», поэтому +1 мс)
	sig := func(ms float64) float32 {                                  // «речь» 1 с через 1 с поверх тихого фона
		if int(ms/1000)%2 == 0 {
			return float32(0.1 * math.Sin(ms*1.3))
		}
		nl := c.noise
		if nl == 0 {
			nl = 2e-4
		}
		return float32(nl * 2 * (rng.Float64() - 0.5))
	}
	nextMic, nextRef := c.micP, 10.0
	refHoldUntil := 0.0
	var out []float32
	padding := 0.0
	lastT := 0.0
	var lats []float64
	tms := 0.0
	for tms < c.secs*1000 {
		tms += 1 + rng.Float64()*c.jitterMs
		// пакеты, пришедшие к этому моменту (приходят через ~0.5 мс после конца)
		for nextMic+0.5 <= tms {
			n := int(c.micP * 48)
			p := make([]float32, n)
			st := nextMic - c.micP
			for i := range p {
				p[i] = sig(st + float64(i)/48)
			}
			mic.Push([][]float32{p}, qpcOf(st))
			nextMic += c.micP
		}
		if c.refStall > 0 && refHoldUntil < tms && rng.Float64() < c.refStall/1.5 {
			refHoldUntil = tms + 30
		}
		for nextRef+0.5 <= tms && tms >= refHoldUntil {
			st := nextRef - 10
			if !(st >= c.refGap[0]*1000 && st < c.refGap[1]*1000) {
				l := make([]float32, 480)
				r := make([]float32, 480)
				for i := range l {
					l[i] = float32(0.05 * math.Sin(float64(i)))
					r[i] = l[i]
				}
				ref.Push([][]float32{l, r}, qpcOf(st))
			}
			nextRef += 10
		}
		// устройство доигрывает
		padding -= (tms - lastT) * 48 * (1 + drift)
		lastT = tms
		starved := false
		if padding <= 0 {
			if simDebug && tms > 2000 {
				me, _ := mic.End()
				re, _ := ref.End()
				println("starve", int(tms), "micEnd", me, "refEnd", re, "waited", eng.Waited, "q", sink.Queued(), "target", sink.Target, "next", func() int64 { n, _ := eng.Next(); return n }(), "now", IdxFromQPC(qpcOf(tms), t0))
			}
			padding = 0
			starved = true
		}
		nowIdx := IdxFromQPC(qpcOf(tms), t0)
		if c.newLogic {
			eng.Hold = sink.HoldFor(int(padding), Rate*31/1000)
		}
		out = eng.Step(nowIdx, out[:0])
		sink.Feed(out)
		pad := int(padding)
		if starved {
			pad = 0
		}
		sink.Adapt(pad, time.Duration(tms*float64(time.Millisecond)))
		chunk := sink.Take(pad, 4800-pad)
		padding += float64(len(chunk))
		if nx, ok := eng.Next(); ok && tms > 5000 {
			lat := float64(nowIdx-(nx-1)+eng.AlgoDelay()) + padding + float64(sink.Queued())
			lats = append(lats, lat/48)
		}
	}
	var r simRes
	for _, v := range lats {
		r.avgMs += v
	}
	r.avgMs /= float64(len(lats))
	// 95-й перцентиль
	cp := append([]float64(nil), lats...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	r.p95Ms = cp[len(cp)*95/100]
	r.starves = sink.Starves
	r.targetMs = float64(sink.Target) / 48
	r.quietDropMs = float64(sink.QuietDrops) / 48
	return r
}

var simDebug = false

func micBaseMs(p float64) float64 {
	return MicBaseMs(p)
}

func TestLatencySim(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	cases := []struct {
		name string
		c    simCfg
	}{
		{"было: мик 10 мс", simCfg{micP: 10, secs: 40, jitterMs: 1, refDelayM: 20}},
		{"было: мик 10 мс, опора пропадала", simCfg{micP: 10, secs: 40, jitterMs: 1, refDelayM: 20, refGap: [2]float64{10, 25}}},
		{"стало: мик 10 мс", simCfg{micP: 10, newLogic: true, baseMs: micBaseMs(10), secs: 40, jitterMs: 1, refDelayM: 20}},
		{"стало: мик 10 мс, опора пропадала", simCfg{micP: 10, newLogic: true, baseMs: micBaseMs(10), secs: 40, jitterMs: 1, refDelayM: 20, refGap: [2]float64{10, 25}}},
		{"было: мик 10 мс, громкий фон (авто-громкость)", simCfg{micP: 10, secs: 40, jitterMs: 1, refDelayM: 20, refGap: [2]float64{10, 25}, noise: 0.01}},
		{"стало: мик 10 мс, громкий фон, опора пропадала", simCfg{micP: 10, newLogic: true, baseMs: micBaseMs(10), secs: 40, jitterMs: 1, refDelayM: 20, refGap: [2]float64{10, 25}, noise: 0.01}},
		{"было: мик 10 мс, наушники иногда тормозят", simCfg{micP: 10, secs: 40, jitterMs: 1, refDelayM: 20, refStall: 0.002}},
		{"стало: мик 10 мс, наушники иногда тормозят", simCfg{micP: 10, newLogic: true, baseMs: micBaseMs(10), secs: 40, jitterMs: 1, refDelayM: 20, refStall: 0.002}},
		{"стало: мик 3 мс", simCfg{micP: 3, newLogic: true, baseMs: micBaseMs(3), secs: 40, jitterMs: 1, refDelayM: 20}},
		{"стало: мик 2.67 мс", simCfg{micP: 128.0 / 48, newLogic: true, baseMs: micBaseMs(128.0 / 48), secs: 40, jitterMs: 1, refDelayM: 20}},
		{"стало: мик 1 мс", simCfg{micP: 1, newLogic: true, baseMs: micBaseMs(1), secs: 40, jitterMs: 1, refDelayM: 20}},
	}
	for _, cs := range cases {
		r := simChain(cs.c)
		t.Logf("%-36s задержка в среднем %5.1f мс (95%%: %5.1f), провалов %d, запас вывода %4.1f мс, выкинуто в тишине %.0f мс",
			cs.name, r.avgMs, r.p95Ms, r.starves, r.targetMs, r.quietDropMs)
		if cs.c.newLogic && r.starves > 1 {
			t.Errorf("%s: провалов %d", cs.name, r.starves)
		}
	}
}
