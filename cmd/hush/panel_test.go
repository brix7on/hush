package main

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

func demoState() uiState {
	return uiState{Running: true, Found: true, Voice: true, MicDB: -28, RefDB: -22, OutDB: -18, Reduction: 9.4,
		GainDB: 6, Beta: 1.5, Noise: 2, EQ: 1, AGC: true, AGCGain: 4, PTT: 1, PTTKey: "Mouse 5", PTTOpen: true,
		Cue: true, LatencyMs: 28, ExclWant: true, ExclOn: true, MicPeriodMs: 10, Mic: "Headset", Out: "CABLE Input"}
}

func TestPanelPaint(t *testing.T) {
	cv := newCanvas(panelW, panelH)
	hits, texts := paintPanel(cv, demoState(), point{-1, -1}, true)
	if len(hits) < 20 || len(texts) < 5 {
		t.Fatalf("hits %d texts %d", len(hits), len(texts))
	}
	for _, h := range hits {
		if h.r.L < 0 || h.r.T < 0 || h.r.R > panelW || h.r.B > panelH || h.r.R <= h.r.L || h.r.B <= h.r.T {
			t.Fatalf("hit outside panel: %+v", h)
		}
	}
	for _, v := range cv.px {
		if v != cInk && v != cPaper {
			t.Fatalf("third color %06x", v)
		}
	}
	out := os.Getenv("HUSH_PREVIEW")
	if out == "" {
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, panelW, panelH))
	for i, v := range cv.px {
		img.Set(i%panelW, i/panelW, color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255})
	}
	f, _ := os.Create(out + ".png")
	png.Encode(f, img)
	f.Close()
	type op struct {
		L, T, R, B int32
		S          string
		Ink        bool
		Font       int
		Flags      uint32
	}
	var ops []op
	for _, x := range texts {
		ops = append(ops, op{x.r.L, x.r.T, x.r.R, x.r.B, x.s, x.ink, x.font, x.flags})
	}
	b, _ := json.Marshal(ops)
	os.WriteFile(out+".json", b, 0o644)
}
