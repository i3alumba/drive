package videoworker

import (
	"strconv"
	"strings"
)

// Parse only the fixed local EVENT subset produced by this runner. This is not
// the API's media validator or a general purpose HLS parser.
func parseHLSPlaylist(data []byte) ([]int64, bool, error) {
	bad := func() ([]int64, bool, error) { return nil, false, failure("protocol_error") }
	if len(data) == 0 || len(data) > 1<<20 || data[len(data)-1] != '\n' || strings.ContainsAny(string(data), "\r\x00") {
		return bad()
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lines[0] != "#EXTM3U" {
		return bad()
	}
	seen := map[string]bool{}
	var durations []int64
	var pending int64
	ended := false
	for _, line := range lines[1:] {
		if ended {
			return bad()
		}
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			if pending != 0 || !strings.HasSuffix(line, ",") {
				return bad()
			}
			value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			parts := strings.Split(value, ".")
			if len(parts) > 2 || len(parts[0]) == 0 {
				return bad()
			}
			for _, r := range strings.Join(parts, "") {
				if r < '0' || r > '9' {
					return bad()
				}
			}
			seconds, err := strconv.ParseInt(parts[0], 10, 32)
			if err != nil {
				return bad()
			}
			fraction := int64(0)
			if len(parts) == 2 {
				if len(parts[1]) == 0 || len(parts[1]) > 6 {
					return bad()
				}
				fraction, err = strconv.ParseInt(parts[1]+strings.Repeat("0", 6-len(parts[1])), 10, 32)
				if err != nil {
					return bad()
				}
			}
			pending = seconds*1000000 + fraction
			if pending <= 0 || pending > HLSTargetSeconds*1000000 {
				return bad()
			}
		case !strings.HasPrefix(line, "#"):
			name, err := HLSSegmentName(len(durations))
			if err != nil || line != name || pending == 0 {
				return bad()
			}
			durations = append(durations, pending)
			pending = 0
		case line == "#EXT-X-ENDLIST":
			if pending != 0 {
				return bad()
			}
			ended = true
		default:
			if pending != 0 {
				return bad()
			}
			key := strings.SplitN(line, ":", 2)[0]
			if seen[key] {
				return bad()
			}
			seen[key] = true
			switch key {
			case "#EXT-X-VERSION":
				if line != "#EXT-X-VERSION:7" {
					return bad()
				}
			case "#EXT-X-TARGETDURATION":
				v, err := strconv.Atoi(strings.TrimPrefix(line, key+":"))
				if err != nil || v < 1 || v > HLSTargetSeconds {
					return bad()
				}
			case "#EXT-X-MEDIA-SEQUENCE":
				if line != "#EXT-X-MEDIA-SEQUENCE:0" {
					return bad()
				}
			case "#EXT-X-PLAYLIST-TYPE":
				if line != "#EXT-X-PLAYLIST-TYPE:EVENT" {
					return bad()
				}
			case "#EXT-X-INDEPENDENT-SEGMENTS":
				if line != key {
					return bad()
				}
			case "#EXT-X-MAP":
				if line != `#EXT-X-MAP:URI="init.mp4"` {
					return bad()
				}
			default:
				return bad()
			}
		}
	}
	for _, key := range []string{"#EXT-X-VERSION", "#EXT-X-TARGETDURATION", "#EXT-X-MEDIA-SEQUENCE", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-MAP", "#EXT-X-INDEPENDENT-SEGMENTS"} {
		if !seen[key] {
			return bad()
		}
	}
	if pending != 0 {
		return bad()
	}
	var total int64
	for _, d := range durations {
		total += d
		if total > HLSMaxDurationUS {
			return bad()
		}
	}
	return durations, ended, nil
}
