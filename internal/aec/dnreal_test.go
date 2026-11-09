package aec

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestDenoiseReal(t *testing.T) {
	p := os.Getenv("REC_WAV")
	if p == "" {
		t.Skip()
	}
	mic, _, sr := readWAV24(t, p)
	for _, lv := range []int{2, 3, 4} {
		d := NewDenoiser()
		d.SetStrength(lv)
		out := runDn(d, mic)
		var b strings.Builder
		for s := 2; s < len(mic)/sr; s++ {
			fmt.Fprintf(&b, "%d:%.0f→%.0f ", s, dB(power(mic, s*sr, (s+1)*sr)), dB(power(out, s*sr, (s+1)*sr)))
		}
		t.Logf("сила %d: %s", lv, b.String())
	}
}
