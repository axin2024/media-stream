package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/axin2024/media-stream/internal/catalog"
)

func (f FFmpeg) Clip(ctx context.Context, files *Files, workspace string, fragments []catalog.ClipFragment, start, end int64, output string) (int64, int64, error) {
	if len(fragments) == 0 {
		return 0, 0, errors.New("no source segments")
	}
	start = (start + 39) / 40 * 40
	end = min((end+39)/40*40, fragments[len(fragments)-1].MediaStart+int64(fragments[len(fragments)-1].Duration))
	if end-start < 40 {
		return 0, 0, errors.New("trim contains fewer than one video frame")
	}
	cursor := fragments[0].MediaStart
	var manifest strings.Builder
	manifest.WriteString("ffconcat version 1.0\n")
	for index, fragment := range fragments {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		if fragment.MediaStart != cursor {
			return 0, 0, errors.New("trim has a missing source segment")
		}
		cursor += int64(fragment.Duration)
		var source string
		for _, blob := range fragment.Blobs {
			if path, err := files.Path(blob, true); err == nil {
				source = path
				break
			}
		}
		if source == "" {
			return 0, 0, errors.New("all renditions of a source segment are unavailable")
		}
		name := fmt.Sprintf("frag-%05d.ts", index)
		if err := stageMedia(source, filepath.Join(workspace, name)); err != nil {
			return 0, 0, err
		}
		fmt.Fprintf(&manifest, "file '%s'\nduration %s\n", name, ms(fragment.Duration))
	}
	list := filepath.Join(workspace, "clip.ffconcat")
	if err := os.WriteFile(list, []byte(manifest.String()), 0o640); err != nil {
		return 0, 0, err
	}
	progress := &tail{max: 8_192}
	threads := strconv.Itoa(f.Threads)
	args := []string{"-v", "error", "-xerror", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-threads", threads,
		"-filter_threads", threads, "-f", "concat", "-safe", "1", "-i", list,
		"-ss", ms(int(start - fragments[0].MediaStart)), "-t", ms(int(end - start)), "-map", "0:v:0", "-map", "0:a:0?",
		"-map_metadata", "-1", "-vf", "scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=1280:720:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=25",
		"-af", "aresample=48000:async=1:first_pts=0,apad", "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-threads:v", threads, "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-progress", "pipe:1", "-f", "mp4", output}
	if err := f.command(ctx, progress, args...); err != nil {
		return 0, 0, err
	}
	var outputUS int64
	for line := range strings.SplitSeq(progress.String(), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "out_time_us="); ok {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				outputUS = max(parsed, outputUS)
			}
		}
	}
	if outputUS < (end-start-80)*1_000 || outputUS > (end-start+80)*1_000 || outputUS <= 0 {
		return 0, 0, fmt.Errorf("clip duration %d ms outside source interval %d ms", outputUS/1_000, end-start)
	}
	return start, end, nil
}

func stageMedia(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	return errors.Join(err, output.Close())
}
