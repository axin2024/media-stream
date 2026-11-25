package media

import (
	"strings"
	"testing"
)

func TestWebVTTTrimAndRebase(t *testing.T) {
	input := `WEBVTT

pilot
00:00:18.500 --> 00:00:22.600
Opening caption

00:00:39.000 --> 00:00:42.000
Ending caption

00:00:45.900 --> 00:00:56.000
Outside the delivery interval
`
	cues, err := ParseVTT([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	output := VTT(cues, 20_000, 40_000)
	if !strings.Contains(output, "00:00:00.000 --> 00:00:02.600") ||
		!strings.Contains(output, "00:00:19.000 --> 00:00:20.000") || strings.Contains(output, "Outside") {
		t.Fatalf("invalid rebasing: %s", output)
	}
	if _, err := ParseVTT([]byte("WEBVTT\n\n00:19.200 --> 00:18.000\nreverse")); err == nil {
		t.Fatal("reverse caption interval accepted")
	}
}
