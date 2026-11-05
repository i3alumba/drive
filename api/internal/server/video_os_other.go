//go:build !linux

package server

import "os"

func videoOpenRoot(string) (*os.File, error) { return nil, errUploadPlatform }
