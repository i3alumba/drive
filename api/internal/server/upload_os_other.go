//go:build !linux

package server

import (
	"errors"
	"os"
)

var errUploadPlatform = errors.New("chunked upload staging requires Linux")

func uploadOpenDir(*os.File, string) (*os.File, error)       { return nil, errUploadPlatform }
func uploadOpenFile(*os.File, string, int) (*os.File, error) { return nil, errUploadPlatform }
func uploadMkdir(*os.File, string) error                     { return errUploadPlatform }
func uploadRename(*os.File, string, string) error            { return errUploadPlatform }
func uploadRenameNewDir(*os.File, string, string) error      { return errUploadPlatform }
func uploadUnlink(*os.File, string, bool) error              { return errUploadPlatform }
func uploadPrivateDir(*os.File) error                        { return errUploadPlatform }
func uploadTryLock(*os.File) error                           { return errUploadPlatform }
func uploadFreeBytes(*os.File) (int64, error)                { return 0, errUploadPlatform }
