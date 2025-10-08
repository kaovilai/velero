/*
Copyright The Velero Contributors.

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

package kopia

import (
	"io"
	"os"

	"github.com/pkg/errors"
	"github.com/vmware-tanzu/velero/pkg/util/csi"
)

// CBTAwareReader reads only changed blocks from a block device and zero-fills unchanged regions
// This preserves Kopia's content-defined chunking while dramatically reducing source I/O
type CBTAwareReader struct {
	device        *os.File
	changedRanges []csi.ByteRange
	totalSize     int64

	currentPos   int64 // Current position in logical stream
	currentRange int   // Index of current range being read
	rangePos     int64 // Position within current range
}

// NewCBTAwareReader creates a reader that selectively reads changed blocks
func NewCBTAwareReader(device *os.File, ranges []csi.ByteRange, totalSize int64) *CBTAwareReader {
	return &CBTAwareReader{
		device:        device,
		changedRanges: ranges,
		totalSize:     totalSize,
		currentPos:    0,
		currentRange:  0,
		rangePos:      0,
	}
}

// Read implements io.Reader interface
// Returns actual data for changed ranges, zeros for unchanged ranges
func (r *CBTAwareReader) Read(p []byte) (n int, err error) {
	if r.currentPos >= r.totalSize {
		return 0, io.EOF
	}

	bytesToRead := len(p)
	if int64(bytesToRead) > r.totalSize-r.currentPos {
		bytesToRead = int(r.totalSize - r.currentPos)
	}

	// Check if current position is in a changed range
	if r.currentRange < len(r.changedRanges) {
		rng := r.changedRanges[r.currentRange]

		// We're before the current range - zero fill the gap
		if r.currentPos < rng.Offset {
			gapSize := rng.Offset - r.currentPos
			toFill := min(int64(bytesToRead), gapSize)

			// Zero-fill the gap
			for i := int64(0); i < toFill; i++ {
				p[i] = 0
			}

			r.currentPos += toFill
			return int(toFill), nil
		}

		// We're inside the current range - read actual data
		if r.currentPos >= rng.Offset && r.currentPos < rng.Offset+rng.Length {
			// Seek to correct position in device
			deviceOffset := rng.Offset + r.rangePos
			if _, err := r.device.Seek(deviceOffset, io.SeekStart); err != nil {
				return 0, errors.Wrap(err, "failed to seek in device")
			}

			// Read only up to end of current range
			remainingInRange := rng.Length - r.rangePos
			toRead := min(int64(bytesToRead), remainingInRange)

			n, err = r.device.Read(p[:toRead])
			if err != nil && err != io.EOF {
				return n, err
			}

			r.currentPos += int64(n)
			r.rangePos += int64(n)

			// Move to next range if we've exhausted current one
			if r.rangePos >= rng.Length {
				r.currentRange++
				r.rangePos = 0
			}

			return n, nil
		}
	}

	// We're past all ranges - zero fill to end
	toFill := min(int64(bytesToRead), r.totalSize-r.currentPos)
	for i := int64(0); i < toFill; i++ {
		p[i] = 0
	}

	r.currentPos += toFill
	return int(toFill), nil
}

// Close closes the underlying device
func (r *CBTAwareReader) Close() error {
	if r.device != nil {
		return r.device.Close()
	}
	return nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
