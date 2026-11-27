package media

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"golang.org/x/image/font/gofont/goregular"
)

func TestProductionEncodingAndOverlayFrames(t *testing.T) {
	binary := os.Getenv("MEDIA_STREAM_TEST_FFMPEG")
	if binary == "" {
		t.Skip("set MEDIA_STREAM_TEST_FFMPEG to run real FFmpeg")
	}
	probe := "ffprobe"
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	ff := FFmpeg{Path: binary, ProbePath: filepath.Join(filepath.Dir(binary), probe), Threads: 2}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	directory := t.TempDir()
	fontPath := filepath.Join(directory, "regular.ttf")
	encodingCheck(t, os.WriteFile(fontPath, goregular.TTF, 0600))
	graphics := Graphics{FontPath: fontPath}
	files, err := OpenFiles(directory)
	encodingCheck(t, err)
	defer files.Close()

	makeInput := func(name string, frequency int) catalog.FrameInput {
		t.Helper()
		path := filepath.Join(directory, name+".ts")
		encodingCheck(t, ff.run(ctx, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c="+name+":s=1280x720:r=25:d=6",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=f=%d:r=48000:d=6", frequency), "-c:v", "libx264", "-threads:v", "2",
			"-preset", "ultrafast", "-bf", "0", "-c:a", "aac", "-ar", "48000", "-f", "mpegts", path))
		encodingCheck(t, ff.VerifyProductionInput(ctx, path, 150, "stereo"))
		blob, err := files.SavePath(path, MaxFragmentBytes)
		encodingCheck(t, err)
		return catalog.FrameInput{Blob: blob, Left: 150, Audio: "stereo"}
	}
	red, blue := makeInput("red", 440), makeInput("blue", 880)
	preview := catalog.OverlayRequest{Template: "lower_third", Content: "Studio guest", Size: 32, Length: 125}
	_, digest, err := graphics.Preview(preview)
	encodingCheck(t, err)
	overlay := catalog.Overlay{Template: preview.Template, Content: preview.Content, Size: preview.Size,
		Font: hex.EncodeToString(digest[:]), On: 2, Off: 127}
	highlight := catalog.ProductionJob{Preset: "standard", Frames: 132, Plan: catalog.FramePlan{Input: red, Overlays: []catalog.Overlay{overlay}}}
	timeline := productionPixel(t, ctx, ff, files, highlight, "crop=2:2:48:600")
	for frame := range 132 {
		pixel := timeline[frame*12 : frame*12+3]
		isRed := pixel[0] > 150 && pixel[0] > pixel[1]+40
		if isRed != (frame < 2 || frame >= 127) {
			t.Fatalf("overlay visible at wrong frame %d (pixel %v)", frame, pixel)
		}
	}
	switching := catalog.ProductionJob{Start: 132, Preset: "standard", Frames: 35,
		Plan: catalog.FramePlan{Input: blue, Previous: &red, FadeFrames: 10}}
	timeline = productionPixel(t, ctx, ff, files, switching, "crop=2:2:100:100")
	start, middle, end := timeline[:3], timeline[5*12:5*12+3], timeline[15*12:15*12+3]
	if start[0] < 190 || start[2] > 30 || middle[0] < 50 || middle[2] < 50 || end[0] > 30 || end[2] < 190 {
		t.Fatalf("unexpected dissolve colors %v, %v and %v", start, middle, end)
	}
	slate := catalog.ProductionJob{Start: 167, Preset: "standard", Frames: 1,
		Plan: catalog.FramePlan{Input: catalog.FrameInput{BuiltIn: true, Audio: "mute"}}}
	productionPixel(t, ctx, ff, files, slate, "crop=2:2:100:100")
}

func productionPixel(t *testing.T, ctx context.Context, ff FFmpeg, files *Files, job catalog.ProductionJob, crop string) []byte {
	t.Helper()
	fontPath := filepath.Join(files.path, "regular.ttf")
	rendered, err := ff.RenderProduction(ctx, files, Graphics{FontPath: fontPath}, job)
	encodingCheck(t, err)
	large := rendered[len(rendered)-1]
	if large.Stats.Track != "720p" || large.Stats.VideoFirst != 90_000+job.Start*3_600 {
		t.Fatalf("unexpected output timestamps: %+v", large.Stats)
	}
	path, err := files.Path(large.Blob, true)
	encodingCheck(t, err)
	command := exec.CommandContext(ctx, ff.Path, "-v", "error", "-nostdin", "-threads", "2", "-i", path,
		"-vf", crop+",format=rgb24", "-an", "-c:v", "rawvideo", "-threads:v", "1", "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
	hideConsole(command)
	out, err := command.Output()
	if failure, ok := err.(*exec.ExitError); ok {
		t.Fatal(string(failure.Stderr))
	}
	encodingCheck(t, err)
	if len(out) != job.Frames*12 {
		t.Fatalf("decoded %d bytes for %d 2×2 frames", len(out), job.Frames)
	}
	return out
}

func encodingCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
