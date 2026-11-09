package aec

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestChainReal(t *testing.T) {
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
	var c0, c1 int
	fmt.Sscanf(os.Getenv("CAL"), "%d:%d", &c0, &c1)
	for _, lv := range []int{0, 2, 4} {
		s := NewSuppressor(sr)
		d := NewDenoiser()
		d.SetStrength(lv)
		H := s.H
		out := make([]float64, len(mic))
		rb, mb, ob := make([]float32, H), make([]float32, H), make([]float32, H)
		step := 0
		for st := 0; st+H <= len(mic); st += H {
			if step == c0*sr/H {
				s.StartCalib((c1 - c0) * sr / H)
			}
			for i := 0; i < H; i++ {
				rb[i], mb[i] = float32(ref[st+i]), float32(mic[st+i])
			}
			s.Process(rb, mb, ob)
			d.VoiceHint(s.VoiceLevel())
			d.Process(ob)
			for i := 0; i < H; i++ {
				if j := st + i - (s.N - s.H) - (d.N - d.H); j >= 0 {
					out[j] = float64(ob[i])
				}
			}
			step++
		}
		var b strings.Builder
		for sec := 0; sec < len(mic)/sr; sec++ {
			fmt.Fprintf(&b, "%d:%.0f→%.0f ", sec, dB(power(mic, sec*sr, (sec+1)*sr)), dB(power(out, sec*sr, (sec+1)*sr)))
		}
		t.Logf("шумодав %d: %s", lv, b.String())
	}
}
