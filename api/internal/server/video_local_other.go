//go:build !linux

package server

import "errors"

func newLocalVideoProcessor(VideoPreviewOptions) (videoProcessor, error) {
	return nil, errors.New("local video execution requires Linux")
}
