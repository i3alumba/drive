package server

import (
	"mime/multipart"
	"net/http"
	"testing"
)

func TestTorrentConversionFlagStrictSingleLiteral(t *testing.T) {
	for _, tc := range []struct {
		name          string
		values        []string
		want, invalid bool
	}{
		{"legacy", nil, false, false}, {"off", []string{"false"}, false, false}, {"on", []string{"true"}, true, false},
		{"duplicate", []string{"true", "true"}, false, true}, {"conflict", []string{"true", "false"}, false, true},
		{"numeric", []string{"1"}, false, true}, {"case", []string{"TRUE"}, false, true}, {"empty", []string{""}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{MultipartForm: &multipart.Form{Value: map[string][]string{"convertVideos": tc.values}}}
			got, err := torrentConversionFlag(r)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("got %v %v", got, err)
			}
		})
	}
}
