package catalog

import (
	"fmt"
	"slices"
)

const (
	ProductionFPS       = 25
	ProductionFrameMS   = 1000 / ProductionFPS
	ProductionAudioRate = 48_000
	ProductionTimebase  = 90_000
	ProductionMaxFrames = 24 * 60 * 60 * ProductionFPS
)

type ProductionInputError struct {
	Field string `json:"field"`
	Cause string `json:"cause"`
}

func (e *ProductionInputError) Error() string { return e.Field + ": " + e.Cause }

type RundownItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Slot     string `json:"slot_id"`
	Kind     string `json:"kind"`
	In       int64  `json:"in_frame"`
	Out      int64  `json:"out_frame"`
	SourceIn int64  `json:"source_in_frame"`
	Fixed    bool   `json:"fixed"`
}

type RundownConflict struct {
	Item    string `json:"item_id"`
	Against string `json:"against_id"`
	By      int64  `json:"overlap_frames"`
}

type RundownChange struct {
	Expected uint64        `json:"expected_revision"`
	Items    []RundownItem `json:"items"`
	Extend   string        `json:"extend_item,omitempty"`
	By       int64         `json:"extension_frames,omitempty"`
}

type Rundown struct {
	Revision uint64            `json:"rundown_rev"`
	Items    []RundownItem     `json:"items"`
	Conflict []RundownConflict `json:"conflict_items"`
}

func calculateRundown(items []RundownItem, extend string, by int64) (Rundown, error) {
	result := Rundown{Items: slices.Clone(items), Conflict: []RundownConflict{}}
	if len(items) == 0 || len(items) > 200 {
		return result, &ProductionInputError{"items", "between 1 and 200 items are required"}
	}
	var end int64
	seen := make(map[string]bool, len(items))
	for index, item := range items {
		if !identifier.MatchString(item.ID) || seen[item.ID] || !identifier.MatchString(item.Slot) || !strictText(item.Title, 160) ||
			(item.Kind != "live" && item.Kind != "clip" && item.Kind != "slate" && item.Kind != "advert") ||
			item.In < end || item.Out <= item.In || item.Out > ProductionMaxFrames || item.SourceIn < 0 ||
			item.SourceIn > ProductionMaxFrames || (item.Kind == "advert" && !item.Fixed) {
			return result, &ProductionInputError{fmt.Sprintf("items[%d]", index), "check item identity, source, order, range and fixed advert placement"}
		}
		end = item.Out
		seen[item.ID] = true
	}
	if extend == "" && by == 0 {
		return result, nil
	}
	if !seen[extend] || by <= 0 || by > 60*60*ProductionFPS {
		return result, &ProductionInputError{"extension_frames", "a live item and an extension between one frame and one hour are required"}
	}
	index := slices.IndexFunc(items, func(item RundownItem) bool { return item.ID == extend })
	if items[index].Fixed || items[index].Kind != "live" {
		return result, &ProductionInputError{"extend_item", "only a movable live item can be extended"}
	}
	result.Items[index].Out += by
	for n := index + 1; n < len(items); n++ {
		if !items[n].Fixed {
			result.Items[n].In += by
			result.Items[n].Out += by
		}
	}
	for n := index; n < len(items); n++ {
		left := result.Items[n]
		if left.Out > ProductionMaxFrames {
			return result, &ProductionInputError{"extension_frames", "the extended production exceeds 24 hours"}
		}
		for _, right := range result.Items[n+1:] {
			overlap := min(left.Out, right.Out) - max(left.In, right.In)
			if overlap > 0 {
				result.Conflict = append(result.Conflict, RundownConflict{left.ID, right.ID, overlap})
			} else if right.In < left.In {
				result.Conflict = append(result.Conflict, RundownConflict{left.ID, right.ID, 0})
			}
		}
	}
	return result, nil
}
