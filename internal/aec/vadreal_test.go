package aec

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// VAD на реальных записях: REC_WAV, CAL=от:до (с), где ты молчишь.
func TestVADReal(t *testing.T) {
	p := os.Getenv("REC_WAV")
	if p == "" {
		t.Skip()
	}
	mic, refs, sr := readWAV24(t, p)
	ref := make([]float64, len(mic))
	for i := range ref {
		for _, r := range refs {
			ref[i] += r[i] / float64(len(refs))
		}
	}
	if v := os.Getenv("VADP"); v != "" {
		fmt.Sscanf(v, "%g:%g:%g", &vadNfMul, &vadStart, &vadFull)
	}
	var c0, c1 int
	fmt.Sscanf(os.Getenv("CAL"), "%d:%d", &c0, &c1)
	s := NewSuppressor(sr)
	H := s.H
	rb, mb, ob := make([]float32, H), make([]float32, H), make([]float32, H)
	perSec := map[int][2]int{}
	step := 0
	for st := 0; st+H <= len(mic); st += H {
		if step == c0*sr/H {
			s.StartCalib((c1 - c0) * sr / H)
		}
		for i := 0; i < H; i++ {
			rb[i], mb[i] = float32(ref[st+i]), float32(mic[st+i])
		}
		s.Process(rb, mb, ob)
		sec := st / sr
		v := perSec[sec]
		v[1]++
		if s.Voice() {
			v[0]++
		}
		perSec[sec] = v
		step++
	}
	var b strings.Builder
	for sec := 0; sec < len(mic)/sr; sec++ {
		v := perSec[sec]
		fmt.Fprintf(&b, "%d:%d%% ", sec, 100*v[0]/(v[1]+1))
	}
	t.Logf("калибровка found=%v; голос по секундам: %s", s.Calib().Found, b.String())
}
