package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/png"
	"math"
	"strings"
)

// ---------- общие типы панели ----------

type point struct{ X, Y int32 }
type rect struct{ L, T, R, B int32 }

func (r rect) has(p point) bool { return p.X >= r.L && p.X < r.R && p.Y >= r.T && p.Y < r.B }

type uiState struct {
	MicDB, RefDB, OutDB float64
	Reduction           float64
	Voice               bool
	Calibrating, Found  bool
	Running             bool
	Limiting            float64
	GainDB, Beta        float64
	Noise, EQ           int
	AGC                 bool
	AGCGain             float64
	PTT                 int
	PTTKey              string
	PTTOpen, Binding    bool
	Cue                 bool
	LatencyMs           int
	ExclWant, ExclOn    bool
	MicPeriodMs         float64
	MicExWant, MicExOn  bool
	MicExWhy            string
	MicGlitches         int
	OutStarves          int
	MicOwnClock         bool
	MicLossPct          float64
	Mic, Ref, Out       string
	Note                string
	UpdLatest, UpdMsg   string
	UpdBusy             bool
}

// action — просьба интерфейса к основной программе.
type action struct {
	Kind string
	S    string
	F    float64
}

type hitBox struct {
	r   rect
	act action
	tip string
}

const (
	fontTitle = iota
	fontBody
	fontSmall
	fontMono
	fontCount
)

const (
	dtLeft     = 0x0
	dtCenter   = 0x1
	dtRight    = 0x2
	dtVCenter  = 0x4
	dtSingle   = 0x20
	dtEllipsis = 0x8000
)

type textOp struct {
	r     rect
	s     string
	ink   bool
	font  int
	flags uint32
}

func noteOf(st uiState) string {
	switch {
	case st.Note != "":
		return st.Note
	case !st.Running:
		return L("переподключение", "reconnecting")
	case st.Binding:
		return L("нажми кнопку для микрофона", "press a key for the mic")
	case st.PTT != 0 && !st.PTTOpen:
		return L("микрофон выключен", "mic is off")
	case st.Calibrating:
		return L("калибровка", "calibrating")
	case !st.Found:
		return L("подстройка под гарнитуру", "learning the headset")
	case st.Voice:
		return L("говоришь", "speaking")
	}
	return L("тишина", "silence")
}

// ---------- холст ----------

const (
	panelW = 600
	panelH = 580

	cInk   uint32 = 0x0a0a0b
	cPaper uint32 = 0xded8ca
)

// canvas — буфер пикселей; рисование идёт в координатах панели, f — масштаб, ox/oy — сдвиг колонки.
type canvas struct {
	w, h   int
	px     []uint32
	f      float64
	ox, oy int
}

func newCanvas(w, h int) *canvas { return &canvas{w: w, h: h, px: make([]uint32, w*h), f: 1} }

func (c *canvas) dev(v int32) int { return int(float64(v)*c.f + 0.5) }

// devRect переводит прямоугольник панели в пиксели холста.
func (c *canvas) devRect(r rect) rect {
	return rect{int32(c.ox + c.dev(r.L)), int32(c.oy + c.dev(r.T)), int32(c.ox + c.dev(r.R)), int32(c.oy + c.dev(r.B))}
}

func (c *canvas) toPanel(x, y int32) point {
	return point{int32(float64(int(x)-c.ox) / c.f), int32(float64(int(y)-c.oy) / c.f)}
}

func (c *canvas) fillDev(r rect, col uint32) {
	x0, y0, x1, y1 := max(0, int(r.L)), max(0, int(r.T)), min(c.w, int(r.R)), min(c.h, int(r.B))
	for y := y0; y < y1; y++ {
		row := c.px[y*c.w : (y+1)*c.w]
		for x := x0; x < x1; x++ {
			row[x] = col
		}
	}
}

func (c *canvas) set(x, y int, col uint32) {
	c.fillDev(c.devRect(rect{int32(x), int32(y), int32(x + 1), int32(y + 1)}), col)
}

func (c *canvas) fill(r rect, col uint32) { c.fillDev(c.devRect(r), col) }

func (c *canvas) frame(r rect, col uint32) {
	c.fill(rect{r.L, r.T, r.R, r.T + 1}, col)
	c.fill(rect{r.L, r.B - 1, r.R, r.B}, col)
	c.fill(rect{r.L, r.T, r.L + 1, r.B}, col)
	c.fill(rect{r.R - 1, r.T, r.R, r.B}, col)
}

// dots — точечная заливка, как узор --dots в Gust.
func (c *canvas) dots(r rect, col uint32) {
	for y := r.T; y < r.B; y += 2 {
		for x := r.L; x < r.R; x += 2 {
			c.set(int(x), int(y), col)
		}
	}
}

func (c *canvas) box(r rect) {
	c.fill(r, cInk)
	c.frame(r, cPaper)
}

func (c *canvas) glyph(name string, cx, cy int32, col uint32) {
	g := glyphs[name]
	if len(g) == 0 {
		return
	}
	x0, y0 := int(cx)-len(g[0])/2, int(cy)-len(g)/2
	for y, row := range g {
		for x := 0; x < len(row); x++ {
			if row[x] == '#' {
				c.set(x0+x, y0+y, col)
			}
		}
	}
}

// bars — значок уровня: n из m столбиков.
func (c *canvas) bars(n, m int, cx, cy int32, col uint32) {
	const bw, gap = 3, 2
	total := m*bw + (m-1)*gap
	x := int(cx) - total/2
	for i := 0; i < m; i++ {
		h := 3 + i*9/max(1, m-1)
		r := rect{int32(x), cy + 6 - int32(h), int32(x + bw), cy + 6}
		if i < n {
			c.fill(r, col)
		} else {
			c.frame(r, col)
		}
		x += bw + gap
	}
}

// ---------- фон ----------

//go:embed scene.png
var scenePNG []byte

var sceneMask []bool

func loadScene() {
	if sceneMask != nil {
		return
	}
	sceneMask = make([]bool, panelW*panelH)
	img, err := png.Decode(bytes.NewReader(scenePNG))
	if err != nil {
		return
	}
	b := img.Bounds()
	for y := 0; y < panelH && y < b.Dy(); y++ {
		for x := 0; x < panelW && x < b.Dx(); x++ {
			r, _, _, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			sceneMask[y*panelW+x] = r > 0x8000
		}
	}
}

// scene заливает весь холст фоном: целое увеличение по высоте, справа внизу, слева повторяется край неба.
func (c *canvas) scene() {
	loadScene()
	k := max(1, (c.h+panelH-1)/panelH)
	x0, y0 := c.w-panelW*k, c.h-panelH*k
	for y := 0; y < c.h; y++ {
		sy := (y - y0) / k
		row := c.px[y*c.w : (y+1)*c.w]
		for x := range row {
			sx := (x - x0) / k
			if x < x0 {
				sx = ((x-x0)/k%64 + 64) % 64
			}
			if sy >= 0 && sx < panelW && sy < panelH && sceneMask[sy*panelW+sx] {
				row[x] = cPaper
			} else {
				row[x] = cInk
			}
		}
	}
}

// ---------- отрисовка ----------

func dbFrac(db float64) float64 { return math.Max(0, math.Min(1, (db+70)/70)) }

// paintPanel рисует панель и возвращает зоны нажатий и подписи для вывода шрифтом системы.
func paintPanel(c *canvas, st uiState, hov point, blink bool) ([]hitBox, []textOp) {
	var hits []hitBox
	var texts []textOp
	tip := ""
	text := func(r rect, s string, ink bool, font int, flags uint32) {
		texts = append(texts, textOp{r, s, ink, font, flags | dtSingle})
	}
	if c.f <= 0 {
		c.f = 1
	}
	pw, ph := c.dev(panelW), c.dev(panelH)
	c.ox, c.oy = 0, max(0, (c.h-ph)/2)
	if extra := c.w - pw; extra > 0 {
		c.ox = min(extra, c.w/25)
	}
	c.scene()

	// шапка
	head := rect{16, 14, 364, 52}
	c.box(head)
	text(rect{28, 14, 240, 52}, "H U S H", false, fontTitle, dtVCenter)
	switch {
	case st.UpdBusy:
		text(rect{180, 14, 352, 52}, st.UpdMsg, false, fontSmall, dtVCenter|dtRight)
	case st.UpdLatest != "":
		r := rect{212, 20, 356, 46}
		hot := r.has(hov)
		c.fill(r, cPaper)
		if hot {
			c.fill(r, cInk)
			c.frame(r, cPaper)
			tip = fmt.Sprintf(L("Скачать и установить %s, hush перезапустится", "Download and install %s, hush will restart"), st.UpdLatest)
		}
		text(r, fmt.Sprintf(L("обновить до %s", "update to %s"), st.UpdLatest), !hot, fontSmall, dtVCenter|dtCenter)
		hits = append(hits, hitBox{r, action{Kind: "update"}, ""})
	default:
		text(rect{200, 14, 352, 52}, Version, false, fontSmall, dtVCenter|dtRight)
	}

	// состояние
	stat := rect{16, 60, 364, 96}
	c.box(stat)
	lamp := rect{28, 71, 42, 85}
	switch {
	case !st.Running:
		c.frame(lamp, cPaper)
		c.frame(rect{lamp.L + 2, lamp.T + 2, lamp.R - 2, lamp.B - 2}, cPaper)
	case st.Calibrating || st.Binding:
		if blink {
			c.fill(lamp, cPaper)
		} else {
			c.frame(lamp, cPaper)
			c.dots(lamp, cPaper)
		}
	case st.PTT != 0 && !st.PTTOpen:
		c.frame(lamp, cPaper)
		for i := int32(0); i < 14; i++ {
			c.set(int(lamp.L+i), int(lamp.T+i), cPaper)
			c.set(int(lamp.R-1-i), int(lamp.T+i), cPaper)
		}
	case st.Voice:
		c.fill(lamp, cPaper)
	default:
		c.frame(lamp, cPaper)
	}
	text(rect{54, 60, 352, 96}, noteOf(st), false, fontBody, dtVCenter|dtEllipsis)

	// индикаторы
	meters := rect{16, 104, 364, 216}
	c.box(meters)
	meter := func(y int32, ic string, db float64) {
		c.glyph(ic, 34, y, cPaper)
		bar := rect{56, y - 5, 352, y + 6}
		c.frame(bar, cPaper)
		if w := int32(float64(bar.R-bar.L-4) * dbFrac(db)); w > 0 {
			c.fill(rect{bar.L + 2, bar.T + 2, bar.L + 2 + w, bar.B - 2}, cPaper)
		}
	}
	meter(124, "mic", st.MicDB)
	meter(150, "phones", st.RefDB)
	meter(176, "plug", st.OutDB)
	if st.Limiting > 1 {
		c.dots(rect{352 - 30, 170, 352, 182}, cPaper)
	}
	line := ""
	switch {
	case !st.Running:
	case st.Calibrating:
		line = L("Не говори, идёт калибровка", "Stay quiet, calibrating")
	case !st.Found:
		line = L("Включи звук в наушниках, hush подстроится", "Play something in the headphones to let hush adapt")
	case st.RefDB < -80:
		line = L("В наушниках тихо", "Headphones are silent")
	default:
		line = fmt.Sprintf(L("Наводка −%.1f дБ", "Bleed −%.1f dB"), math.Abs(st.Reduction))
	}
	statusR := rect{28, 190, 352, 210}

	// ряды настроек
	set := rect{16, 224, 364, 532}
	c.box(set)
	rowIcon := func(y int32, ic, warn string) {
		c.glyph(ic, 34, y+13, cPaper)
		if warn != "" {
			c.glyph("warn", 46, y+5, cPaper)
			if (rect{22, y, 52, y + 26}).has(hov) {
				tip = warn
			}
		}
	}
	button := func(r rect, ic, label string, a action, on bool, tp string) {
		hot := r.has(hov)
		if hot {
			tip = tp
		}
		inv := on != hot
		if inv {
			c.fill(r, cPaper)
		} else {
			c.fill(r, cInk)
		}
		c.frame(r, cPaper)
		fg := cPaper
		if inv {
			fg = cInk
		}
		switch {
		case ic != "" && label != "":
			c.glyph(ic, r.L+18, (r.T+r.B)/2, fg)
			text(rect{r.L + 30, r.T, r.R - 6, r.B}, label, inv, fontMono, dtVCenter|dtEllipsis)
		case ic != "":
			c.glyph(ic, (r.L+r.R)/2, (r.T+r.B)/2, fg)
		default:
			text(r, label, inv, fontMono, dtVCenter|dtCenter|dtEllipsis)
		}
		hits = append(hits, hitBox{r, a, tp})
	}
	seg := func(y int32, ic string, glyphsN []string, tips []string, sel int, kind, warn string) {
		rowIcon(y, ic, warn)
		cr := rect{56, y, 352, y + 26}
		c.frame(cr, cPaper)
		n := int32(len(glyphsN))
		bw := (cr.R - cr.L) / n
		for i, g := range glyphsN {
			l := cr.L + int32(i)*bw
			r := rect{l, cr.T, l + bw, cr.B}
			if int32(i) == n-1 {
				r.R = cr.R
			}
			if i > 0 {
				c.fill(rect{r.L, r.T, r.L + 1, r.B}, cPaper)
			}
			fg := cPaper
			if i == sel {
				c.fill(rect{r.L + 2, r.T + 2, r.R - 2, r.B - 2}, cPaper)
				fg = cInk
			} else if r.has(hov) {
				c.frame(rect{r.L + 2, r.T + 2, r.R - 2, r.B - 2}, cPaper)
			}
			if r.has(hov) {
				tip = tips[i]
			}
			cx, cy := (r.L+r.R)/2, (r.T+r.B)/2
			if strings.HasPrefix(g, "lvl:") {
				var a, b int
				fmt.Sscanf(g, "lvl:%d:%d", &a, &b)
				c.bars(a, b, cx, cy, fg)
			} else {
				c.glyph(g, cx, cy, fg)
			}
			hits = append(hits, hitBox{r, action{Kind: kind, F: float64(i)}, tips[i]})
		}
	}
	const y0, dy = 236, 32
	rowY := func(i int) int32 { return int32(y0 + i*dy) }

	rowIcon(rowY(0), "speaker", "")
	text(rect{56, rowY(0), 240, rowY(0) + 26}, fmt.Sprintf(L("%+.0f дБ", "%+.0f dB"), st.GainDB), false, fontMono, dtVCenter)
	button(rect{256, rowY(0), 302, rowY(0) + 26}, "minus", "", action{Kind: "gain", F: -1}, false, L("Усиление −1 дБ", "Gain −1 dB"))
	button(rect{306, rowY(0), 352, rowY(0) + 26}, "plus", "", action{Kind: "gain", F: +1}, false, L("Усиление +1 дБ", "Gain +1 dB"))

	betaSel := 1
	switch {
	case st.Beta < 1.25:
		betaSel = 0
	case st.Beta > 1.75:
		betaSel = 2
	}
	seg(rowY(1), "phones", []string{"lvl:1:3", "lvl:2:3", "lvl:3:3"},
		[]string{L("Подавление наводки: слабое", "Bleed suppression: soft"), L("Подавление наводки: обычное", "Bleed suppression: normal"), L("Подавление наводки: сильное", "Bleed suppression: strong")},
		betaSel, "beta", "")
	seg(rowY(2), "waves", []string{"off", "lvl:1:4", "lvl:2:4", "lvl:3:4", "lvl:4:4"},
		[]string{L("Шумоподавление выключено", "Noise reduction off"), L("Шумоподавление: слабое", "Noise reduction: light"), L("Шумоподавление: среднее", "Noise reduction: medium"),
			L("Шумоподавление: сильное", "Noise reduction: strong"), L("Шумоподавление: максимум", "Noise reduction: maximum")},
		st.Noise, "noise", "")
	seg(rowY(3), "sliders", []string{"off", "speech", "soft"},
		[]string{L("Эквалайзер выключен", "Equalizer off"), L("Эквалайзер: чёткая речь", "Equalizer: clear speech"), L("Эквалайзер: мягкий", "Equalizer: soft")},
		st.EQ, "eq", "")
	agcSel := 0
	if st.AGC {
		agcSel = 1
	}
	seg(rowY(4), "auto", []string{"off", "auto"},
		[]string{L("Автогромкость выключена", "Auto gain off"), fmt.Sprintf(L("Автогромкость включена (%+.0f дБ)", "Auto gain on (%+.0f dB)"), st.AGCGain)},
		agcSel, "agc", "")
	seg(rowY(5), "mic", []string{"mic", "hold", "toggle"},
		[]string{L("Микрофон всегда включён", "Mic always on"), L("Микрофон работает, пока нажата кнопка", "Push to talk"), L("Кнопка включает и выключает микрофон", "Key toggles the mic")},
		st.PTT, "ptt", "")

	rowIcon(rowY(6), "key", "")
	keyLabel := st.PTTKey
	if st.Binding {
		keyLabel = L("нажми клавишу", "press a key")
	}
	button(rect{56, rowY(6), 302, rowY(6) + 26}, "", keyLabel, action{Kind: "bind"}, st.Binding, L("Назначить кнопку микрофона", "Assign the mic key"))
	button(rect{306, rowY(6), 352, rowY(6) + 26}, "speaker", "", action{Kind: "cue"}, st.Cue, L("Звук при включении микрофона", "Sound on mic toggle"))

	micSel, exSel := 0, 1
	if st.MicExWant {
		micSel = 1
	}
	if st.ExclWant {
		exSel = 0
	}
	micWarn, exWarn := "", ""
	if st.Running && st.MicPeriodMs > 0 {
		switch {
		case st.MicExWant && !st.MicExOn:
			micWarn = L("Монопольный захват не включился", "Exclusive capture is not active")
			if st.MicExWhy != "" {
				micWarn += ": " + st.MicExWhy
			}
		case st.MicGlitches > 0 || st.MicLossPct >= 0.1:
			micWarn = fmt.Sprintf(L("Сбоев микрофона: %d, потери %.1f%%", "Mic glitches: %d, loss %.1f%%"), st.MicGlitches, st.MicLossPct)
		}
	}
	if st.ExclWant && st.Running && !st.ExclOn {
		exWarn = L("Монопольный вывод не включился, работает обычный", "Exclusive output is not active, shared mode is used")
	}
	seg(rowY(7), "capture", []string{"shared", "lock"},
		[]string{L("Захват микрофона: общий", "Mic capture: shared"), L("Захват микрофона: монопольный", "Mic capture: exclusive")}, micSel, "micexcl", micWarn)
	seg(rowY(8), "plug", []string{"lock", "shared"},
		[]string{L("Вывод в кабель: монопольный", "Cable output: exclusive"), L("Вывод в кабель: общий", "Cable output: shared")}, exSel, "excl", exWarn)

	// кнопки
	button(rect{16, 542, 188, 568}, "calib", L("Калибровка", "Calibrate"), action{Kind: "recal"}, false, L("Заново измерить наводку", "Measure the bleed again"))
	button(rect{192, 542, 364, 568}, "tray", L("Свернуть", "Hide"), action{Kind: "hidepanel"}, false, L("Свернуть в трей", "Hide to tray"))

	// строка подсказки
	if tip != "" {
		text(statusR, tip, false, fontSmall, dtVCenter|dtEllipsis)
	} else {
		text(rect{statusR.L, statusR.T, 260, statusR.B}, line, false, fontSmall, dtVCenter|dtEllipsis)
		if st.Running && st.LatencyMs > 0 {
			lat := fmt.Sprintf(L("~%d мс", "~%d ms"), st.LatencyMs)
			if st.OutStarves > 0 {
				lat += fmt.Sprintf(L(" · сбоев %d", " · %d dropouts"), st.OutStarves)
			}
			text(rect{240, statusR.T, statusR.R, statusR.B}, lat, false, fontSmall, dtVCenter|dtRight)
		}
	}
	return hits, texts
}
