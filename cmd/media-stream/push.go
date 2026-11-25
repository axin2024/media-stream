package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/axin2024/media-stream/internal/media"
)

var sessionID = regexp.MustCompile(`^[0-9a-f]{24}$`)

func pushCommand(ctx context.Context, log *slog.Logger, flags *flag.FlagSet, args []string) error {
	origin := flags.String("origin", "http://127.0.0.1:8080", "HTTPS origin or loopback HTTP origin")
	session := flags.String("session", "", "session ID from the opening receipt")
	source := flags.String("source", "", "input file or supported FFmpeg stream URL")
	duration := flags.Int("segment-ms", 4_000, "session fragment duration")
	clock := flags.Int64("clock-start-ms", 0, "source clock at the start of this input")
	engine := flags.String("ffmpeg", "ffmpeg", "FFmpeg executable")
	threads := flags.Int("threads", 2, "FFmpeg threads")
	fast := flags.Bool("fast", false, "process file input without simulating real time")
	lost := flags.Bool("notify-lost", false, "report this input as lost after its source ends")
	if err := parse(flags, args); err != nil {
		return err
	}
	target, err := sourceURL(*origin, *session)
	if err != nil {
		return err
	}
	token := os.Getenv("MEDIA_STREAM_PUSH_TOKEN")
	if len(token) != 64 || *source == "" || *duration < 1_000 || *duration > 10_000 || *duration%1_000 != 0 ||
		*clock < 0 || *clock > math.MaxInt64-172_800_000 || *threads < 1 || *threads > 16 {
		return errors.New("set the push token, source, and the session's segment duration")
	}
	if strings.Contains(*engine, "/") || strings.Contains(*engine, "\\") {
		absolute, err := filepath.Abs(*engine)
		if err != nil {
			return err
		}
		*engine = absolute
	}
	dir, err := os.MkdirTemp("", "media-stream-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	encoder := media.FFmpeg{Path: *engine, Threads: *threads}
	complete := make(chan error, 1)
	go func() { complete <- encoder.Partition(processCtx, *source, dir, *duration, *fast) }()

	client := &http.Client{Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	sender := sourceSender{Client: client, URL: target, Token: token, Root: dir, Duration: *duration, Clock: *clock, Log: log}
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			<-complete
			return ctx.Err()
		case err := <-complete:
			if sendErr := sender.deliver(ctx); sendErr != nil {
				return sendErr
			}
			if *lost {
				if notifyErr := sender.lost(ctx); notifyErr != nil {
					return errors.Join(err, notifyErr)
				}
			}
			return err
		case <-timer.C:
			if err := sender.deliver(ctx); err != nil {
				stop()
				<-complete
				return err
			}
		}
	}
}

func sourceURL(origin, session string) (string, error) {
	value, err := url.Parse(origin)
	if err != nil || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" ||
		!sessionID.MatchString(session) || (value.Scheme != "https" &&
		!(value.Scheme == "http" && (value.Hostname() == "localhost" || net.ParseIP(value.Hostname()).IsLoopback()))) {
		return "", errors.New("origin must use HTTPS or loopback HTTP and a valid session ID")
	}
	return strings.TrimRight(value.String(), "/") + "/api/sessions/" + session, nil
}

type sourceSender struct {
	Client   *http.Client
	URL      string
	Token    string
	Root     string
	Duration int
	Clock    int64
	Next     int
	Log      *slog.Logger
}

func (p *sourceSender) deliver(ctx context.Context) error {
	data, err := os.ReadFile(filepath.Join(p.Root, "segments.csv"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Ignore the final partial line if FFmpeg was still publishing it.
	last := strings.LastIndexByte(string(data), '\n')
	if last < 0 {
		return nil
	}
	reader := csv.NewReader(strings.NewReader(string(data[:last+1])))
	reader.FieldsPerRecord = 3
	for index := 0; ; index++ {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if index < p.Next {
			continue
		}
		name := fmt.Sprintf("part-%08d.ts", index)
		from, fromErr := strconv.ParseFloat(row[1], 64)
		to, toErr := strconv.ParseFloat(row[2], 64)
		if row[0] != name || fromErr != nil || toErr != nil || to <= from {
			return errors.New("unexpected segment index from FFmpeg")
		}
		duration := min(int(math.Round((to-from)*1_000)), p.Duration)
		if duration >= 100 {
			if err := p.put(ctx, name, index, duration); err != nil {
				return err
			}
		}
		p.Next++
		if err := os.Remove(filepath.Join(p.Root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}

func (p *sourceSender) put(ctx context.Context, name string, index, duration int) error {
	for attempt := range 12 {
		retry, err := p.send(ctx, name, index, duration)
		if err == nil {
			return nil
		}
		if !retry || attempt == 11 {
			return err
		}
		timer := time.NewTimer(time.Duration(min(attempt+1, 3)) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("source delivery exhausted retries")
}

func (p *sourceSender) send(ctx context.Context, name string, index, duration int) (bool, error) {
	file, err := os.Open(filepath.Join(p.Root, name))
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL+"/input", file)
	if err != nil {
		return false, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "video/mp2t")
	req.Header.Set("X-Source-Time-Ms", strconv.FormatInt(p.Clock+int64(index)*int64(p.Duration), 10))
	req.Header.Set("X-Source-Duration-Ms", strconv.Itoa(duration))
	res, err := p.Client.Do(req)
	if err != nil {
		return ctx.Err() == nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8_192))
		return false, nil
	}
	var failure struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 8_192)).Decode(&failure)
	if res.StatusCode == http.StatusConflict && failure.Code == "fenced" {
		p.Log.Info("discard expired source fragment", "sequence", index)
		return false, nil
	}
	retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
	return retry, fmt.Errorf("input fragment %d returned %s (%s)", index, res.Status, failure.Code)
}

func (p *sourceSender) lost(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/input-lost", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	res, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusConflict {
		return fmt.Errorf("input-lost report returned %s", res.Status)
	}
	return nil
}
