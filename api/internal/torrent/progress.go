package torrent

import (
	"regexp"
	"strconv"
	"strings"
)

var aria2ProgressPattern = regexp.MustCompile(`([0-9.]+)\s*([KMGT]?i?B)/([0-9.]+)\s*([KMGT]?i?B)\(([0-9]+)%\).*DL:([0-9.]+)\s*([KMGT]?i?B)(?:/s)?(?:.*ETA:([0-9hms]+))?`)

func parseAria2Progress(line string) (progress float64, speedBytesPerSecond int64, etaSeconds int64, ok bool) {
	match := aria2ProgressPattern.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, 0, false
	}
	completedBytes, err := parseByteSize(match[1], match[2])
	if err != nil {
		return 0, 0, 0, false
	}
	totalBytes, err := parseByteSize(match[3], match[4])
	if err != nil {
		return 0, 0, 0, false
	}
	percent, err := strconv.ParseFloat(match[5], 64)
	if err != nil {
		return 0, 0, 0, false
	}
	speed, err := parseByteSize(match[6], match[7])
	if err != nil {
		return 0, 0, 0, false
	}
	eta := int64(0)
	if match[8] != "" {
		eta = parseDurationSeconds(match[8])
	} else if totalBytes > completedBytes && speed > 0 {
		eta = (totalBytes - completedBytes) / speed
		if (totalBytes-completedBytes)%speed != 0 {
			eta++
		}
	}
	return percent / 100, speed, eta, true
}

func parseByteSize(value string, unit string) (int64, error) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	multipliers := map[string]float64{
		"B":   1,
		"KB":  1000,
		"MB":  1000 * 1000,
		"GB":  1000 * 1000 * 1000,
		"TB":  1000 * 1000 * 1000 * 1000,
		"KiB": 1024,
		"MiB": 1024 * 1024,
		"GiB": 1024 * 1024 * 1024,
		"TiB": 1024 * 1024 * 1024 * 1024,
	}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, strconv.ErrSyntax
	}
	return int64(number * multiplier), nil
}

func parseDurationSeconds(value string) int64 {
	parts := regexp.MustCompile(`([0-9]+)([hms])`).FindAllStringSubmatch(strings.TrimSpace(value), -1)
	seconds := int64(0)
	for _, part := range parts {
		number, err := strconv.ParseInt(part[1], 10, 64)
		if err != nil {
			continue
		}
		switch part[2] {
		case "h":
			seconds += number * 3600
		case "m":
			seconds += number * 60
		case "s":
			seconds += number
		}
	}
	return seconds
}
