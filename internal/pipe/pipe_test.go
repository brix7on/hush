package pipe

import (
	"math"
	"math/rand"
	"testing"

	"github.com/brix7on/hush/internal/aec"
)

func speech(rng *rand.Rand, n int, amp float64) []float64 {
	out := make([]float64, n)
	var lp1, lp2 float64
	for i := range out {
		w := rng.NormFloat64()
		lp1 += 0.25 * (w - lp1)
		lp2 += 0.6 * (lp1 - lp2)
		t := float64(i) / Rate
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3.1*t)
		env *= 0.5 + 0.5*math.Sin(2*math.Pi*0.37*t+1)
		out[i] = amp * env * (lp1*2 + lp2*3)
	}
	return out
}

func pw(x []float64, a, b int) float64 {
	var s float64
	for i := a; i < b; i++ {
		s += x[i] * x[i]
	}
	return s / float64(b-a)
}

type pkt struct {
	at   int // время доставки, мс
	a    int // индекс первого отсчёта
	data []float32
}

// Полная симуляция: пакеты по 10 мс с джиттером, тишина в наушниках (пакетов нет!),
// разговор поверх, задержка эха 45 мс.
func runSim(t *testing.T, offMs, secs int) (a1, a2, a3, snr float64, eng *Engine) {
	rng := rand.New(rand.NewSource(3))
	n := secs * Rate
	ref := speech(rng, n, 0.25)
	for i := 4 * Rate; i < 7*Rate; i++ { // тишина в наушниках 4..7 с
		ref[i] = 0
	}
	const delay = 45 * Rate / 1000
	h := make([]float64, delay+2000)
	h[delay] = 0.35
	for k := 1; k < 2000; k++ {
		h[delay+k] = 0.35 * 0.5 * rng.NormFloat64() * math.Exp(-float64(k)/400)
	}
	echo := make([]float64, n)
	for i := range echo {
		var s float64
		for k, hk := range h {
			if i-k >= 0 {
				s += hk * ref[i-k]
			}
		}
		echo[i] = s
	}
	near := make([]float64, n)
	nz := speech(rand.New(rand.NewSource(9)), n, 0.2)
	for i := 12 * Rate; i < 16*Rate; i++ {
		near[i] = nz[i]
	}
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = echo[i] + near[i] + 0.0005*rng.NormFloat64()
	}

	mk := func(sig []float64, latMs, jit int, skipSilent bool) []pkt {
		var ps []pkt
		last := 0
		for a := 0; a+480 <= n; a += 480 {
			d := make([]float32, 480)
			nz := false
			for i := range d {
				d[i] = float32(sig[a+i])
				if d[i] != 0 {
					nz = true
				}
			}
			if skipSilent && !nz {
				continue
			}
			at := a/48 + 10 + latMs + rng.Intn(jit+1)
			if at < last {
				at = last
			}
			last = at
			ps = append(ps, pkt{at, a, d})
		}
		return ps
	}
	micP := mk(mic, 15, 3, false)
	refP := mk(ref, 5, 3, true)

	micS := NewStreamIn(Rate*2, 0, 0)
	refS := NewStreamIn(Rate*2, 0, 960)
	off := int64(offMs) * Rate / 1000
	eng = NewEngine(micS, refS, aec.New(aec.Config{}))
	var out []float32
	mi, ri := 0, 0
	stamp := func(a int) int64 { return int64(a)*10000000/Rate + 1 } // метка 0 = «неизвестно»
	for ms := 0; ms < secs*1000; ms++ {
		for mi < len(micP) && micP[mi].at <= ms {
			micS.Push([][]float32{micP[mi].data}, stamp(micP[mi].a))
			mi++
		}
		for ri < len(refP) && refP[ri].at <= ms {
			refS.Push([][]float32{refP[ri].data}, stamp(refP[ri].a)+off*10000000/Rate)
			ri++
		}
		out = eng.Step(int64(ms)*48, out)
	}
	o := make([]float64, n)
	for i, v := range out {
		idx := int(eng.First) + i
		if idx < n {
			o[idx] = float64(v)
		}
	}
	att := func(a, b int) float64 {
		return 10 * math.Log10(pw(mic, a*Rate, b*Rate)/pw(o, a*Rate, b*Rate))
	}
	t.Logf("блоков %d, ожиданий опоры %d, ресинков ref=%d mic=%d", eng.Blocks, eng.Waited, refS.ResyncCount(), micS.ResyncCount())
	a1, a2, a3 = att(2, 4), att(9, 12), att(secs-6, secs)
	t.Logf("подавление: 2-4с %.1f дБ | 9-12с %.1f дБ | последние 6с %.1f дБ; поправок задержки: %d", a1, a2, a3, eng.Corrections)
	var e float64
	for i := 13 * Rate; i < 16*Rate; i++ {
		d := o[i] - near[i]
		e += d * d
	}
	e /= float64(3 * Rate)
	snr = 10 * math.Log10(pw(near, 13*Rate, 16*Rate)/e)
	t.Logf("разговор поверх: речь/искажения %.1f дБ", snr)
	return
}

func TestEngineSim(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	a1, a2, a3, snr, _ := runSim(t, 0, 24)
	if a1 < 25 || a2 < 25 || a3 < 25 {
		t.Errorf("недостаточное подавление")
	}
	if snr < 12 {
		t.Errorf("искажена речь")
	}
}

// Метки времени опоры «врут» на 80 мс — программа должна сама найти и поправить.
func TestAutoDelay(t *testing.T) {
	_, _, a3, _, eng := runSim(t, 80, 40)
	if eng.Corrections < 1 || a3 < 25 {
		t.Errorf("не подстроилась: поправок %d, подавление %.1f дБ", eng.Corrections, a3)
	}
}

func TestSinkDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	for _, c := range []struct {
		rate  int
		drift float64
	}{{48000, 300e-6}, {48000, -300e-6}, {44100, 200e-6}, {44100, -200e-6}} {
		s := NewSink(c.rate, 40)
		padding := 0.0
		frac := 0.0
		minFill, maxFill := math.MaxFloat64, 0.0
		under := 0
		in := make([]float32, 48)
		for ms := 0; ms < 300000; ms++ {
			s.Feed(in)
			p := int(padding)
			if got := s.Take(p, 20*c.rate/1000*10-p); len(got) > 0 {
				padding += float64(len(got))
			}
			// устройство «играет» с дрейфом
			frac = float64(c.rate) / 1000 * (1 + c.drift)
			padding -= frac
			if padding < 0 {
				if ms > 500 {
					under++
				}
				padding = 0
			}
			if ms > 2000 {
				f := padding
				minFill, maxFill = math.Min(minFill, f), math.Max(maxFill, f)
			}
		}
		t.Logf("%d Гц дрейф %+.0f ppm: заполненность %.0f..%.0f кадров (цель %d), недобор %d, сбросы %d", c.rate, c.drift*1e6, minFill, maxFill, s.Target, under, s.Drops)
		if under > 0 || s.Drops > 0 {
			t.Errorf("были сбои")
		}
	}
}

func TestStreamInChannels(t *testing.T) {
	s := NewStreamIn(Rate*2, 0, 0)
	mk := func(a int) [][]float32 {
		l, r := make([]float32, 480), make([]float32, 480)
		for i := range l {
			l[i], r[i] = float32(a+i), -float32(a+i)
		}
		return [][]float32{l, r}
	}
	stamp := func(a int) int64 { return int64(a)*10000000/Rate + 1 }
	s.Push(mk(1000), stamp(1000))
	s.Push(mk(1480), stamp(1480))
	s.Push(mk(4000), stamp(4000)) // пауза: должно заполниться нулями
	if s.Channels() != 2 {
		t.Fatalf("каналов %d", s.Channels())
	}
	l, r := make([]float32, 3), make([]float32, 3)
	s.ReadAt(0, 1479, l)
	s.ReadAt(1, 1479, r)
	if l[0] != 1479 || l[1] != 1480 || r[1] != -1480 {
		t.Fatalf("данные: %v %v", l, r)
	}
	s.ReadAt(0, 3000, l)
	if l[0] != 0 {
		t.Fatalf("в паузе должны быть нули: %v", l)
	}
	s.ReadAt(0, 4000, l)
	if l[0] != 4000 {
		t.Fatalf("после паузы: %v", l)
	}
	s.ReadAt(5, 4000, l) // несуществующий канал
	if l[0] != 0 {
		t.Fatal("нет канала — нули")
	}
}

func TestExtract(t *testing.T) {
	mic := NewStreamIn(Rate*40, 0, 0)
	ref := NewStreamIn(Rate*40, 0, 0)
	stamp := func(a int) int64 { return int64(a)*10000000/Rate + 1 }
	// 12 с: микрофон — непрерывно; наушники 8 каналов (играют только 0 и 1), пакеты перестают приходить на 9-й секунде
	for a := 0; a < 12*Rate; a += 480 {
		m := make([]float32, 480)
		for i := range m {
			m[i] = float32(0.01 * math.Sin(float64(a+i)*0.02))
		}
		mic.Push([][]float32{m}, stamp(a))
		if a < 9*Rate {
			chs := make([][]float32, 8)
			for c := range chs {
				chs[c] = make([]float32, 480)
			}
			for i := 0; i < 480; i++ {
				chs[0][i] = float32(0.3 * math.Sin(float64(a+i)*0.05))
				chs[1][i] = float32(0.2 * math.Sin(float64(a+i)*0.07))
			}
			ref.Push(chs, stamp(a))
		}
	}
	chans, notes, err := Extract(mic, ref, int64(12*Rate))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		t.Log(n)
	}
	if len(chans) != 3 {
		t.Fatalf("ожидалось 3 канала (микрофон + 2 активных), получено %d", len(chans))
	}
	n := len(chans[0])
	if n < 9*Rate || n > 11*Rate {
		t.Fatalf("длина %d", n)
	}
	for c := range chans {
		if len(chans[c]) != n {
			t.Fatal("разная длина каналов")
		}
	}
	// выравнивание: первый отсчёт отрезка — это индекс Rate (первая секунда пропущена)
	want := float32(0.3 * math.Sin(float64(Rate)*0.05))
	if math.Abs(float64(chans[1][0]-want)) > 1e-6 {
		t.Fatalf("сдвиг опоры: %v != %v", chans[1][0], want)
	}
	wantM := float32(0.01 * math.Sin(float64(Rate)*0.02))
	if math.Abs(float64(chans[0][0]-wantM)) > 1e-6 {
		t.Fatalf("сдвиг микрофона")
	}
	// после 9-й секунды в наушниках — нули (дозаполнено)
	if chans[1][n-1] != 0 {
		t.Fatalf("хвост должен быть тишиной")
	}
}

// Сквозной тест со стереоопорой: наводка только от левого канала, Engine с K=2.
func TestEngineStereo(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	rng := rand.New(rand.NewSource(5))
	secs := 20
	n := secs * Rate
	L := speech(rng, n, 0.25)
	R := speech(rand.New(rand.NewSource(6)), n, 0.25)
	const delay = 40 * Rate / 1000
	h := make([]float64, delay+1500)
	h[delay] = 0.02
	for k := 1; k < 1500; k++ {
		h[delay+k] = 0.02 * 0.4 * rng.NormFloat64() * math.Exp(-float64(k)/300)
	}
	mic := make([]float64, n)
	for i := range mic {
		var s float64
		for k, hk := range h {
			if i-k >= 0 {
				s += hk * L[i-k]
			}
		}
		mic[i] = s + 0.0004*rng.NormFloat64()
	}
	micS := NewStreamIn(Rate*2, 0, 0)
	refS := NewStreamIn(Rate*2, 0, 960)
	eng := NewEngine(micS, refS, aec.New(aec.Config{Refs: 2}))
	var out []float32
	stamp := func(a int) int64 { return int64(a)*10000000/Rate + 1 }
	for a := 0; a+480 <= n; a += 480 {
		m := make([]float32, 480)
		l := make([]float32, 480)
		r := make([]float32, 480)
		for i := range m {
			m[i], l[i], r[i] = float32(mic[a+i]), float32(L[a+i]), float32(R[a+i])
		}
		micS.Push([][]float32{m}, stamp(a))
		refS.Push([][]float32{l, r}, stamp(a))
		out = eng.Step(int64(a+480), out)
	}
	o := make([]float64, n)
	for i, v := range out {
		if idx := int(eng.First) + i; idx < n {
			o[idx] = float64(v)
		}
	}
	att := 10 * math.Log10(pw(mic, 14*Rate, 20*Rate)/pw(o, 14*Rate, 20*Rate))
	t.Logf("стерео, сквозной: подавление %.1f дБ, сбросов предохранителя %d, поправок %d", att, eng.F.Stats().Resets, eng.Corrections)
	if att < 15 {
		t.Errorf("мало: %.1f дБ", att)
	}
}

// Сквозной тест подавителя: наводка «по огибающей», калибровка шипением, потом речь друга.
func TestEngineSuppressor(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	rng := rand.New(rand.NewSource(17))
	calN, runN := 8*Rate, 16*Rate
	n := calN + runN
	ref := make([]float64, n)
	for i := Rate; i < calN-Rate; i++ {
		if (i*1000/Rate)%800 < 400 {
			ref[i] = 0.05 * rng.NormFloat64()
		}
	}
	fr := speech(rand.New(rand.NewSource(18)), runN, 0.25)
	for i := range fr {
		ref[calN+i] = fr[i]
	}
	// наводка: шум, модулированный огибающей опоры, через 55 мс
	leak := make([]float64, n)
	var env, lp1, lp2 float64
	envs := make([]float64, n)
	for i := range ref {
		env += 0.004 * (math.Abs(ref[i]) - env)
		envs[i] = env
	}
	d := 55 * Rate / 1000
	for i := range leak {
		w := rng.NormFloat64()
		lp1 += 0.3 * (w - lp1)
		lp2 += 0.05 * (lp1 - lp2)
		if i >= d {
			leak[i] = 0.12 * envs[i-d] * (lp1 - lp2)
		}
	}
	mic := make([]float64, n)
	for i := range mic {
		mic[i] = leak[i] + 0.0003*rng.NormFloat64()
	}
	micS := NewStreamIn(Rate*2, 0, 0)
	refS := NewStreamIn(Rate*2, 0, 960)
	sp := aec.NewSuppressor(Rate)
	eng := NewEngineS(micS, refS, sp, 2)
	var out []float32
	stamp := func(a int) int64 { return int64(a)*10000000/Rate + 1 }
	calStarted := false
	for a := 0; a+480 <= n; a += 480 {
		m, l, r := make([]float32, 480), make([]float32, 480), make([]float32, 480)
		for i := range m {
			m[i], l[i], r[i] = float32(mic[a+i]), float32(ref[a+i]), float32(ref[a+i])
		}
		micS.Push([][]float32{m}, stamp(a))
		refS.Push([][]float32{l, r}, stamp(a))
		if !calStarted && a >= Rate/2 {
			sp.StartCalib((calN - Rate) / sp.H)
			calStarted = true
		}
		out = eng.Step(int64(a+480), out)
	}
	c := sp.Calib()
	o := make([]float64, n)
	for i, v := range out {
		if idx := int(eng.First) + i - (sp.N - sp.H); idx >= 0 && idx < n {
			o[idx] = float64(v)
		}
	}
	red := 10 * math.Log10(pw(mic, calN+2*Rate, n)/pw(o, calN+2*Rate, n))
	t.Logf("калибровка: найдено=%v задержка=%.0f мс (с учётом запаса опоры 20 мс) | наводка тише на %.1f дБ",
		c.Found, float64(c.D*sp.H)*1000/Rate, red)
	if !c.Found || red < 8 {
		t.Errorf("не сработало")
	}
}

func TestAmp(t *testing.T) {
	a := NewAmp(0)
	x := []float32{0.1, -0.5, 0.3}
	y := append([]float32(nil), x...)
	a.Process(y)
	for i := range x {
		if x[i] != y[i] {
			t.Fatal("0 дБ должно ничего не менять")
		}
	}
	// +12 дБ: тихий сигнал усиливается в 4 раза, громкий упирается в потолок
	a = NewAmp(12)
	q := make([]float32, Rate)
	for i := range q {
		q[i] = float32(0.05 * math.Sin(float64(i)*0.05))
	}
	a.Process(q)
	var mx float64
	for _, v := range q[Rate/2:] {
		mx = math.Max(mx, math.Abs(float64(v)))
	}
	if math.Abs(mx-0.2) > 0.005 {
		t.Fatalf("тихий: пик %.3f, ожидалось 0.2", mx)
	}
	l := make([]float32, Rate)
	for i := range l {
		l[i] = float32(0.6 * math.Sin(float64(i)*0.05))
	}
	a.Process(l)
	mx = 0
	for _, v := range l {
		mx = math.Max(mx, math.Abs(float64(v)))
	}
	t.Logf("громкий при +12 дБ: пик %.3f (потолок %.2f), ограничитель %.1f дБ", mx, a.Ceil, a.Limiting())
	if mx > a.Ceil+1e-6 {
		t.Fatalf("клиппинг: %.3f", mx)
	}
}
