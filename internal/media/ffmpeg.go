package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/axin2024/media-stream/internal/catalog"
)

type FFmpeg struct {
	Path      string
	ProbePath string
	Threads   int
}

func (f FFmpeg) Check(ctx context.Context) error {
	return f.run(ctx, "-version")
}

func (f FFmpeg) Probe(ctx context.Context, path string) error {
	progress := &tail{max: 8_192}
	args := []string{"-v", "error", "-xerror", "-nostdin", "-protocol_whitelist", "file,pipe",
		"-threads", strconv.Itoa(f.Threads), "-probesize", "1048576", "-analyzeduration", "1000000",
		"-f", "mpegts", "-i", path, "-map", "0:v:0", "-t", "0.32", "-progress", "pipe:1", "-f", "null", "-"}
	if err := f.command(ctx, progress, args...); err != nil {
		return err
	}
	for line := range strings.SplitSeq(progress.String(), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "frame="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
				return nil
			}
		}
	}
	return errors.New("no decodable video frames in source fragment")
}

func (f FFmpeg) Transcode(ctx context.Context, input string, job catalog.Job, profile catalog.Profile, output string) error {
	filter := fmt.Sprintf("setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=25,tpad=stop_mode=clone:stop_duration=0.25",
		profile.Width, profile.Height, profile.Width, profile.Height)
	args := []string{"-v", "error", "-xerror", "-nostdin", "-y", "-protocol_whitelist", "file,pipe",
		"-threads", strconv.Itoa(f.Threads), "-filter_threads", strconv.Itoa(f.Threads),
		"-f", "mpegts", "-i", input, "-map", "0:v:0", "-map", "0:a:0?", "-map_metadata", "-1",
		"-vf", filter, "-af", "asetpts=PTS-STARTPTS,aresample=48000:async=1:first_pts=0,apad",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-threads:v", strconv.Itoa(f.Threads),
		"-profile:v", "main", "-g", strconv.Itoa(job.SegmentMS / 40), "-keyint_min", strconv.Itoa(job.SegmentMS / 40),
		"-sc_threshold", "0", "-bf", "0", "-b:v", fmt.Sprintf("%dk", profile.VideoKbps),
		"-maxrate", fmt.Sprintf("%dk", profile.VideoKbps), "-bufsize", fmt.Sprintf("%dk", profile.VideoKbps*2),
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", profile.AudioKbps),
		"-t", ms(job.DurationMS), "-output_ts_offset", ms(int(job.MediaStartMS)),
		"-muxdelay", "0", "-muxpreload", "0", "-mpegts_flags", "+resend_headers", "-f", "mpegts", output}
	return f.run(ctx, args...)
}

func ms(value int) string {
	return fmt.Sprintf("%d.%03d", value/1_000, value%1_000)
}

func (f FFmpeg) run(ctx context.Context, args ...string) error {
	return f.command(ctx, io.Discard, args...)
}

func (f FFmpeg) command(ctx context.Context, stdout io.Writer, args ...string) error {
	if f.Path == "" || f.Threads < 1 || f.Threads > 16 {
		return errors.New("set the FFmpeg executable and a thread count from 1 to 16")
	}
	cmd := exec.CommandContext(ctx, f.Path, args...)
	hideConsole(cmd)
	diagnostic := &tail{max: 4_096}
	cmd.Stderr, cmd.Stdout = diagnostic, stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Surface only the process status in API responses. Log output may include local file paths.
		return fmt.Errorf("%s: %w: %s", filepath.Base(f.Path), err, strings.TrimSpace(diagnostic.String()))
	}
	return nil
}

type tail struct {
	bytes.Buffer
	max int
}

func (b *tail) Write(p []byte) (int, error) {
	total := len(p)
	if len(p) >= b.max {
		b.Reset()
		_, err := b.Buffer.Write(p[len(p)-b.max:])
		return total, err
	}
	if excess := b.Len() + len(p) - b.max; excess > 0 {
		b.Next(excess)
	}
	_, err := b.Buffer.Write(p)
	return total, err
}
