package media

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Cue struct {
	Start int64
	End   int64
	Text  string
}

var cueTime = regexp.MustCompile(`^(?:([0-9]{2,3}):)?([0-5][0-9]):([0-5][0-9])\.([0-9]{3})$`)

func timecode(value string) (int64, bool) {
	match := cueTime.FindStringSubmatch(value)
	if len(match) == 0 {
		return 0, false
	}
	hours, _ := strconv.ParseInt(match[1], 10, 64)
	minutes, _ := strconv.ParseInt(match[2], 10, 64)
	seconds, _ := strconv.ParseInt(match[3], 10, 64)
	fraction, _ := strconv.ParseInt(match[4], 10, 64)
	return ((hours*60+minutes)*60+seconds)*1_000 + fraction, true
}

func ParseVTT(content []byte) ([]Cue, error) {
	if !utf8.Valid(content) || len(content) > 1<<20 || strings.ContainsRune(string(content), '\x00') {
		return nil, errors.New("invalid WebVTT text")
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(string(content), "\ufeff"), "\r\n", "\n"))
	header, body, _ := strings.Cut(normalized, "\n")
	if header != "WEBVTT" || strings.ContainsRune(body, '\r') {
		return nil, errors.New("expected a WebVTT header")
	}
	cues := make([]Cue, 0)
	for block := range strings.SplitSeq(strings.TrimSpace(body), "\n\n") {
		if block == "" || block == "NOTE" || strings.HasPrefix(block, "NOTE ") || strings.HasPrefix(block, "NOTE\n") {
			continue
		}
		row, text, _ := strings.Cut(block, "\n")
		if !strings.Contains(row, "-->") {
			row, text, _ = strings.Cut(text, "\n")
		}
		left, right, ok := strings.Cut(row, "-->")
		endParts := strings.Fields(right)
		if !ok || len(endParts) == 0 || text == "" {
			return nil, errors.New("invalid WebVTT cue")
		}
		start, startOK := timecode(strings.TrimSpace(left))
		end, endOK := timecode(endParts[0])
		if !startOK || !endOK || end <= start || (len(cues) > 0 && start < cues[len(cues)-1].Start) || len(cues) >= 20_000 {
			return nil, errors.New("invalid or out-of-order WebVTT cue interval")
		}
		cues = append(cues, Cue{Start: start, End: end, Text: text})
	}
	return cues, nil
}

func writeTime(out *strings.Builder, value int64) {
	fmt.Fprintf(out, "%02d:%02d:%02d.%03d", value/3_600_000, value/60_000%60, value/1_000%60, value%1_000)
}

// VTT trims cues to [from,to) and makes from the new player time zero.
func VTT(cues []Cue, from, to int64) string {
	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		if cue.End <= from || cue.Start >= to {
			continue
		}
		writeTime(&out, max(from, cue.Start)-from)
		out.WriteString(" --> ")
		writeTime(&out, min(to, cue.End)-from)
		out.WriteByte('\n')
		out.WriteString(cue.Text)
		out.WriteString("\n\n")
	}
	return out.String()
}
