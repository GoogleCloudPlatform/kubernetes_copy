/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logrotation

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const timeLayout string = "20060102-150405"

type rotationFile struct {
	// required, the max size of the log file in bytes, 0 means no rotation
	maxSize int64
	// required, the max age of the log file, 0 means no cleanup
	maxAge      time.Duration
	filePath    string
	mut         sync.Mutex
	file        *os.File
	currentSize int64
	lasSyncTime time.Time
	enableFlush bool
}

func Open(filePath string, enableFlush bool, maxSize int64, maxAge time.Duration) (io.WriteCloser, error) {
	w := &rotationFile{
		filePath:    filePath,
		maxSize:     maxSize,
		maxAge:      maxAge,
		enableFlush: enableFlush,
	}

	logFile, err := os.OpenFile(w.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	w.file = logFile

	w.lasSyncTime = time.Now()

	if w.maxSize > 0 {
		info, err := os.Stat(w.filePath)
		if err != nil {
			return nil, err
		}
		w.currentSize = info.Size()
	}

	return w, nil
}

// write func to satisfy io.writer interface
func (w *rotationFile) Write(p []byte) (n int, err error) {
	w.mut.Lock()
	defer w.mut.Unlock()

	n, err = w.file.Write(p)
	if err != nil {
		return 0, err
	}

	if w.enableFlush && time.Since(w.lasSyncTime) >= time.Second*5 {
		err = w.file.Sync()
		if err != nil {
			return 0, err
		}
		w.lasSyncTime = time.Now()
	}

	if w.maxSize > 0 {
		w.currentSize += int64(len(p))

		// if file size over maxsize rotate the log file
		if w.currentSize >= w.maxSize {
			// Explicitly call file.Sync() to ensure data is written to disk
			err = w.file.Sync()
			if err != nil {
				return 0, err
			}
			err = w.rotate()
			if err != nil {
				return 0, err
			}
		}
	}

	return n, nil
}

func (w *rotationFile) rotate() error {
	// Get the file extension
	ext := filepath.Ext(w.filePath)

	// Remove the extension from the filename
	pathWithoutExt := strings.TrimSuffix(w.filePath, ext)

	rotateFilePath := pathWithoutExt + "-" + time.Now().Format(timeLayout) + ext

	if w.filePath == rotateFilePath {
		return nil
	}

	err := w.file.Close()
	if err != nil {
		return err
	}

	err = os.Rename(w.filePath, rotateFilePath)
	if err != nil {
		return err
	}

	w.file, err = os.OpenFile(w.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	w.currentSize = 0

	if w.maxAge > 0 {
		go func() {
			err = w.clean(pathWithoutExt, ext)
		}()
	}

	return nil
}

// Clean up the old log files in the format of
// <basename>-<timestamp><ext>
// This should be safe enough to avoid false deletion
// This will work for multiple restarts of the same program
func (w *rotationFile) clean(pathWithoutExt string, ext string) error {
	ageTime := time.Now().Add(-w.maxAge)

	directory := filepath.Dir(pathWithoutExt)
	basename := filepath.Base(pathWithoutExt) + "-"

	dir, err := os.ReadDir(directory)
	if err != nil {
		return err
	}

	err = nil
	for _, v := range dir {
		if strings.HasPrefix(v.Name(), basename) && strings.HasSuffix(v.Name(), ext) {
			// Remove the prefix and suffix
			trimmed := strings.TrimPrefix(v.Name(), basename)
			trimmed = strings.TrimSuffix(trimmed, ext)

			_, err = time.Parse(timeLayout, trimmed)
			if err == nil {
				info, errInfo := v.Info()
				if errInfo != nil {
					err = errInfo
					// Ignore the error while continue with the next clenup
					continue
				}

				if ageTime.After(info.ModTime()) {
					err = os.Remove(filepath.Join(directory, v.Name()))
					if err != nil {
						// Ignore the error while continue with the next clenup
						continue
					}
				}
			}

		}
	}

	return err
}

func (w *rotationFile) Close() error {
	w.mut.Lock()
	defer w.mut.Unlock()

	// Explicitly call file.Sync() to ensure data is written to disk
	err := w.file.Sync()
	if err != nil {
		return err
	}

	err = w.file.Close()
	if err != nil {
		return err
	}

	return nil
}
