package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/axin2024/media-stream/internal/catalog"
)

const samplesPerFrame = catalog.ProductionAudioRate / catalog.ProductionFPS
const aacPacketSamples = 1024

type productionStream struct {
	Index  int    `json:"index"`
	Type   string `json:"codec_type"`
	Codec  string `json:"codec_name"`
	Rate   string `json:"sample_rate"`
	Clock  string `json:"time_base"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type productionPacket struct {
	Stream int    `json:"stream_index"`
	PTS    *int64 `json:"pts"`
	Flags  string `json:"flags"`
}

func (f FFmpeg) probeProduction(ctx context.Context, path string) (catalog.ProductionTrack, error) {
	var track catalog.ProductionTrack
	binary := f.ProbePath
	if binary == "" {
		binary = "ffprobe"
	}
	cmd := exec.CommandContext(ctx, binary, "-v", "error", "-protocol_whitelist", "file,pipe", "-f", "mpegts", "-i", path,
		"-show_entries", "stream=index,codec_type,codec_name,sample_rate,time_base,width,height:packet=stream_index,pts,flags", "-of", "json")
	hideConsole(cmd)
	out := &tail{max: 2 << 20}
	problem := &tail{max: 4096}
	cmd.Stdout, cmd.Stderr = out, problem
	if err := cmd.Run(); err != nil {
		return track, fmt.Errorf("probe production fragment: %w: %s", err, strings.TrimSpace(problem.String()))
	}
	var data struct {
		Streams []productionStream `json:"streams"`
		Packets []productionPacket `json:"packets"`
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return track, err
	}
	var video, audio *productionStream
	for _, stream := range data.Streams {
		if stream.Type == "video" && video == nil {
			video = &stream
		} else if stream.Type == "audio" && audio == nil {
			audio = &stream
		}
	}
	if video == nil || video.Codec != "h264" || video.Clock != "1/90000" ||
		video.Width < 160 || video.Width > 3840 || video.Height < 90 || video.Height > 2160 {
		return track, errors.New("expected H.264 MPEG-TS video with a 90 kHz clock")
	}
	if audio != nil {
		if audio.Clock != "1/90000" || audio.Codec != "aac" {
			return track, errors.New("expected AAC MPEG-TS audio")
		}
		track.AudioRate, _ = strconv.Atoi(audio.Rate)
	}
	for _, packet := range data.Packets {
		if packet.PTS == nil {
			return track, errors.New("MPEG-TS packet has no presentation timestamp")
		}
		pts := *packet.PTS
		switch {
		case packet.Stream == video.Index:
			if track.VideoCount == 0 {
				if !strings.Contains(packet.Flags, "K") {
					return track, errors.New("video does not begin on a key frame")
				}
				track.VideoFirst = pts
			} else if pts-track.VideoLast != catalog.ProductionTimebase/catalog.ProductionFPS {
				return track, errors.New("video timestamps are not a continuous 25 fps clock")
			}
			track.VideoLast = pts
			track.VideoCount++
		case audio != nil && packet.Stream == audio.Index:
			if track.AudioCount == 0 {
				track.AudioFirst = pts
			} else if pts <= track.AudioLast {
				return track, errors.New("audio timestamps do not advance")
			}
			track.AudioLast = pts
			track.AudioCount++
		}
	}
	return track, nil
}

func (f FFmpeg) VerifyProductionInput(ctx context.Context, path string, frames int, audio string) error {
	track, err := f.probeProduction(ctx, path)
	if err != nil {
		return err
	}
	if track.VideoCount != frames || (audio != "mute" && (track.AudioCount == 0 || track.AudioRate < 8000)) {
		return errors.New("declared frame count or required source audio is missing")
	}
	return nil
}

func (f FFmpeg) ProductionFragmentFrames(ctx context.Context, path string) (int, error) {
	track, err := f.probeProduction(ctx, path)
	if err != nil {
		return 0, err
	}
	if track.VideoCount < 1 || track.VideoCount > 250 {
		return 0, errors.New("production fragment must contain between 1 and 250 video frames")
	}
	return track.VideoCount, nil
}

func (f FFmpeg) RenderProduction(ctx context.Context, files *Files, graphics Graphics, job catalog.ProductionJob) ([]catalog.RenderedTrack, error) {
	directory, cleanup, err := files.Workspace()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args := []string{"-v", "error", "-xerror", "-nostdin", "-y", "-filter_complex_threads", strconv.Itoa(f.Threads)}
	selected, err := productionMediaArgs(files, job.Plan.Input, f.Threads)
	if err != nil {
		return nil, err
	}
	args = append(args, selected...)
	graph := productionAV(0, job.Plan.Input, job.Frames, "next")
	video, audio, inputCount := "nextv", "nexta", 1
	if job.Plan.Previous != nil {
		previous, err := productionMediaArgs(files, *job.Plan.Previous, f.Threads)
		if err != nil {
			return nil, err
		}
		args = append(args, previous...)
		graph = append(graph, productionAV(1, *job.Plan.Previous, job.Plan.FadeFrames, "old")...)
		afade := fmt.Sprintf("[olda][nexta]acrossfade=nb_samples=%d[programa]", job.Plan.FadeFrames*samplesPerFrame)
		vfade := fmt.Sprintf("[oldv][nextv]xfade=transition=fade:duration=%s:offset=0[programv]", ms(job.Plan.FadeFrames*catalog.ProductionFrameMS))
		graph = append(graph, afade, vfade)
		video, audio, inputCount = "programv", "programa", 2
	}
	layers, err := graphics.frames(job.Plan.Overlays, job.Start, job.Frames)
	if err != nil {
		return nil, err
	}
	for i, layer := range layers {
		path := filepath.Join(directory, fmt.Sprintf("layer-%d.png", i))
		if err := saveGraphic(path, layer); err != nil {
			return nil, err
		}
		args = append(args, "-protocol_whitelist", "file,pipe", "-framerate", "25", "-i", path)
		label := fmt.Sprintf("layer%d", i)
		graph = append(graph, fmt.Sprintf("[%s][%d:v]overlay=repeatlast=1:enable='between(n,%d,%d)'[%s]", video, inputCount+i, layer.from, layer.until-1, label))
		video = label
	}
	profiles := catalog.Profiles(job.Preset)
	frameZero := job.Start * samplesPerFrame
	startSample := roundPacket(frameZero)
	endSample := roundPacket((job.Start + int64(job.Frames)) * samplesPerFrame)
	packetCount := (endSample - startSample) / aacPacketSamples
	priming := startSample - frameZero + aacPacketSamples
	audioSamples := max(1, (packetCount-1)*aacPacketSamples)
	graph = append(graph,
		fmt.Sprintf("[%s]format=yuv420p,split=2[largesource][smallsource]", video),
		fmt.Sprintf("[%s]atrim=end_sample=%d,asetpts=(N+%d)/SR/TB,asplit=2[a0][a1]", audio, audioSamples, priming),
		fmt.Sprintf("[smallsource]scale=%d:%d[v0]", profiles[0].Width, profiles[0].Height),
		fmt.Sprintf("[largesource]scale=%d:%d[v1]", profiles[1].Width, profiles[1].Height))
	args = append(args, "-filter_complex", strings.Join(graph, ";"))
	paths := make([]string, 0, len(profiles))
	clockOffset := ms(1000 + int(job.Start)*catalog.ProductionFrameMS)
	for index, profile := range profiles {
		path := filepath.Join(directory, profile.Track+".ts")
		paths = append(paths, path)
		args = append(args, "-map", fmt.Sprintf("[v%d]", index), "-map", fmt.Sprintf("[a%d]", index),
			"-c:v", "libx264", "-profile:v", "main", "-preset", "veryfast", "-threads:v", strconv.Itoa(f.Threads),
			"-crf", "22", "-maxrate", fmt.Sprintf("%dk", profile.VideoKbps), "-bufsize", fmt.Sprintf("%dk", profile.VideoKbps*2),
			"-g", strconv.Itoa(job.Frames), "-sc_threshold", "0", "-bf", "0", "-c:a", "aac", "-ac", "2", "-ar", "48000",
			"-b:a", fmt.Sprintf("%dk", profile.AudioKbps), "-bsf:a", fmt.Sprintf(`noise=drop=gte(n\,%d)`, packetCount),
			"-mpegts_copyts", "1", "-avoid_negative_ts", "disabled", "-output_ts_offset", clockOffset,
			"-muxdelay", "0", "-muxpreload", "0", "-mpegts_flags", "+resend_headers", "-f", "mpegts", path)
	}
	if err := f.run(ctx, args...); err != nil {
		return nil, err
	}
	checks := make([]catalog.RenderedTrack, len(profiles))
	for i, path := range paths {
		stats, err := f.probeProduction(ctx, path)
		if err != nil {
			return nil, err
		}
		if stats.VideoCount != job.Frames || stats.AudioCount != int(packetCount) || stats.AudioRate != catalog.ProductionAudioRate {
			return nil, fmt.Errorf("encoder produced %d/%d video frames and %d/%d audio packets for %s",
				stats.VideoCount, job.Frames, stats.AudioCount, packetCount, profiles[i].Track)
		}
		stats.Track = profiles[i].Track
		checks[i].Stats = stats
	}
	for i, path := range paths {
		checks[i].Blob, err = files.SavePath(path, MaxFragmentBytes)
		if err != nil {
			for _, saved := range checks[:i] {
				_ = files.Remove(saved.Blob.Key)
			}
			return nil, err
		}
	}
	return checks, nil
}

func roundPacket(sample int64) int64 {
	return (sample + aacPacketSamples - 1) / aacPacketSamples * aacPacketSamples
}

func productionMediaArgs(files *Files, in catalog.FrameInput, threads int) ([]string, error) {
	if in.BuiltIn {
		return []string{"-f", "lavfi", "-i", "color=c=0x102235:s=1280x720:r=25,drawgrid=w=160:h=120:t=1:c=white@0.13"}, nil
	}
	path, err := files.Path(in.Blob, true)
	if err != nil {
		return nil, err
	}
	args := []string{"-protocol_whitelist", "file,pipe", "-threads", strconv.Itoa(threads)}
	if in.Loop {
		args = append(args, "-stream_loop", "-1")
	}
	return append(args, "-f", "mpegts", "-i", path), nil
}

func productionAV(index int, input catalog.FrameInput, count int, label string) []string {
	v := fmt.Sprintf("[%d:v]fps=25,trim=start_frame=%d:end_frame=%d,setpts=PTS-STARTPTS,"+
		"scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=1280:720:(ow-iw)/2:(oh-ih)/2,setsar=1,settb=1/25,format=yuv444p[%sv]",
		index, input.Offset, input.Offset+int64(count), label)
	source := fmt.Sprintf("[%d:a]", index)
	switch input.Audio {
	case "mute":
		source = "anullsrc=channel_layout=stereo:sample_rate=48000,"
	case "left":
		source += "pan=stereo|c0=c0|c1=c0,"
	case "right":
		source += "aformat=channel_layouts=stereo,pan=stereo|c0=c1|c1=c1,"
	}
	a := fmt.Sprintf("%saresample=48000,aformat=channel_layouts=stereo,apad,atrim=start_sample=%d:end_sample=%d,asetpts=N/SR/TB[%sa]",
		source, input.Offset*samplesPerFrame, (input.Offset+int64(count))*samplesPerFrame, label)
	return []string{v, a}
}

func saveGraphic(path string, graphic graphicFrame) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return errors.Join(png.Encode(f, graphic.image), f.Close())
}

func (f FFmpeg) ProductionPreviewClip(ctx context.Context, source string) (io.Reader, error) {
	var data bytes.Buffer
	err := f.command(ctx, &data, "-v", "error", "-nostdin", "-protocol_whitelist", "file,pipe", "-f", "mpegts", "-i", source,
		"-map", "0:v:0", "-map", "0:a:0?", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-vf", "scale=480:-2",
		"-c:a", "aac", "-ar", "48000", "-t", "3", "-movflags", "+empty_moov+frag_keyframe+default_base_moof", "-f", "mp4", "pipe:1")
	return &data, err
}
