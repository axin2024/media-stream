package media

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func (f FFmpeg) Sample(ctx context.Context, output string, seconds, width, height int) error {
	if seconds < 1 || seconds > 180 || width < 160 || height < 90 || width > 1_920 || height > 1_080 ||
		width%2 != 0 || height%2 != 0 || !strings.EqualFold(filepath.Ext(output), ".mp4") {
		return fmt.Errorf("sample requires MP4, 1 to 180 seconds, and even dimensions from 160x90 to 1920x1080")
	}
	rate := strconv.Itoa(f.Threads)
	return f.run(ctx, "-v", "error", "-nostdin", "-n", "-filter_threads", rate,
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=25", width, height),
		"-f", "lavfi", "-i", "sine=frequency=554:sample_rate=48000",
		"-t", strconv.Itoa(seconds), "-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264",
		"-preset", "veryfast", "-crf", "23", "-threads:v", rate, "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", output)
}

// Partition writes a segment list as FFmpeg closes MPEG-TS files in workspace.
func (f FFmpeg) Partition(ctx context.Context, input, workspace string, segmentMS int, fast bool) error {
	if f.Path == "" || f.Threads < 1 || segmentMS < 1_000 || segmentMS > 10_000 || segmentMS%1_000 != 0 {
		return fmt.Errorf("invalid input encoder settings")
	}
	if !strings.Contains(input, "://") {
		var err error
		input, err = filepath.Abs(input)
		if err != nil {
			return err
		}
	}
	args := []string{"-v", "error", "-nostdin", "-y", "-threads", strconv.Itoa(f.Threads),
		"-filter_threads", strconv.Itoa(f.Threads), "-protocol_whitelist", "file,pipe,rtmp,rtmps,srt,tcp,tls,udp,rist"}
	if !fast && !strings.Contains(input, "://") {
		args = append(args, "-re")
	}
	args = append(args, "-i", input, "-map", "0:v:0", "-map", "0:a:0?", "-map_metadata", "-1",
		"-vf", "fps=25", "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-threads:v", strconv.Itoa(f.Threads), "-g", strconv.Itoa(segmentMS/40), "-keyint_min", strconv.Itoa(segmentMS/40),
		"-sc_threshold", "0", "-bf", "0", "-c:a", "aac", "-b:a", "128k", "-ar", "48000",
		"-f", "segment", "-segment_format", "mpegts", "-segment_format_options", "mpegts_flags=+resend_headers",
		"-segment_time", ms(segmentMS), "-reset_timestamps", "1", "-segment_list", "segments.csv", "-segment_list_type", "csv",
		"part-%08d.ts")
	cmd := exec.CommandContext(ctx, f.Path, args...)
	hideConsole(cmd)
	cmd.Dir = workspace
	diagnostic := &tail{max: 4_096}
	cmd.Stderr, cmd.Stdout = diagnostic, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("source encoder: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return nil
}
