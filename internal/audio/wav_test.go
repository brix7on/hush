package audio

import (
	"bytes"
	"math"
	"os"
	"testing"
)

func TestMixLR(t *testing.T) {
	// 7.1: только FL и FR играют → L=FL, R=FR
	chs := make([][]float32, 8)
	for c := range chs {
		chs[c] = []float32{0}
	}
	chs[0][0], chs[1][0] = 0.5, -0.25
	l, r := MixLR(chs, nil, nil)
	if l[0] != 0.5 || r[0] != -0.25 {
		t.Fatalf("7.1: %v %v", l, r)
	}
	// 5.1: центр уходит в обе стороны
	c6 := [][]float32{{0}, {0}, {1}, {0}, {0}, {0}}
	l, r = MixLR(c6, nil, nil)
	if math.Abs(float64(l[0]-0.707)) > 1e-6 || math.Abs(float64(r[0]-0.707)) > 1e-6 {
		t.Fatalf("5.1 центр: %v %v", l, r)
	}
	// моно
	l, r = MixLR([][]float32{{0.3}}, nil, nil)
	if l[0] != 0.3 || r[0] != 0.3 {
		t.Fatal("моно")
	}
}

func TestWriteWAV24(t *testing.T) {
	n := 48000
	a := make([]float32, n)
	b := make([]float32, n)
	for i := range a {
		a[i] = float32(0.5 * math.Sin(float64(i)*0.05))
		b[i] = float32(0.001 * math.Sin(float64(i)*0.31))
	}
	var buf bytes.Buffer
	if err := WriteWAV24(&buf, 48000, [][]float32{a, b, a}); err != nil {
		t.Fatal(err)
	}
	if want := 12 + 8 + 40 + 8 + n*3*3 - 4 + 4; buf.Len() != want-0 && buf.Len() != 68+n*9 {
		t.Fatalf("размер %d", buf.Len())
	}
	if p := os.Getenv("WAV_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
