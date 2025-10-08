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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/util/csi"
)

// createMockBlockDevice creates a temporary file with test data
func createMockBlockDevice(t *testing.T, data []byte) *os.File {
	t.Helper()
	tmpDir := t.TempDir()
	devicePath := filepath.Join(tmpDir, "mock_device")

	err := os.WriteFile(devicePath, data, 0644)
	require.NoError(t, err)

	device, err := os.Open(devicePath)
	require.NoError(t, err)

	t.Cleanup(func() {
		device.Close()
	})

	return device
}

func TestCBTAwareReader_SingleRange(t *testing.T) {
	// Create a mock device with known data
	// Total size: 1024 bytes, changed range: bytes 100-200
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 100, Length: 100},
	}

	reader := NewCBTAwareReader(device, ranges, 1024)

	// Read all data
	result := make([]byte, 1024)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	// Verify: bytes 0-99 should be zero
	for i := 0; i < 100; i++ {
		assert.Equal(t, byte(0), result[i], "byte %d should be zero", i)
	}

	// Verify: bytes 100-199 should match original data
	for i := 100; i < 200; i++ {
		assert.Equal(t, deviceData[i], result[i], "byte %d should match device data", i)
	}

	// Verify: bytes 200-1023 should be zero
	for i := 200; i < 1024; i++ {
		assert.Equal(t, byte(0), result[i], "byte %d should be zero", i)
	}
}

func TestCBTAwareReader_MultipleRanges(t *testing.T) {
	// Create device with 2048 bytes
	deviceData := make([]byte, 2048)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	// Multiple non-overlapping ranges
	ranges := []csi.ByteRange{
		{Offset: 0, Length: 512},      // First 512 bytes
		{Offset: 1024, Length: 256},   // Middle range
		{Offset: 1800, Length: 248},   // Near end
	}

	reader := NewCBTAwareReader(device, ranges, 2048)

	// Read all data
	result := make([]byte, 2048)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 2048, n)

	// Verify first range (0-511)
	assert.Equal(t, deviceData[0:512], result[0:512])

	// Verify gap (512-1023) is zeros
	for i := 512; i < 1024; i++ {
		assert.Equal(t, byte(0), result[i])
	}

	// Verify second range (1024-1279)
	assert.Equal(t, deviceData[1024:1280], result[1024:1280])

	// Verify gap (1280-1799) is zeros
	for i := 1280; i < 1800; i++ {
		assert.Equal(t, byte(0), result[i])
	}

	// Verify third range (1800-2047)
	assert.Equal(t, deviceData[1800:2048], result[1800:2048])
}

func TestCBTAwareReader_SmallReads(t *testing.T) {
	// Test reading in small chunks (simulating Kopia's chunking behavior)
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 100, Length: 50},
		{Offset: 200, Length: 50},
	}

	reader := NewCBTAwareReader(device, ranges, 1024)

	// Read in 32-byte chunks
	var result bytes.Buffer
	chunk := make([]byte, 32)

	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			result.Write(chunk[:n])
		}
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	finalResult := result.Bytes()
	assert.Equal(t, 1024, len(finalResult))

	// Verify changed ranges have correct data
	assert.Equal(t, deviceData[100:150], finalResult[100:150])
	assert.Equal(t, deviceData[200:250], finalResult[200:250])

	// Verify gaps are zeros
	for i := 0; i < 100; i++ {
		assert.Equal(t, byte(0), finalResult[i])
	}
	for i := 150; i < 200; i++ {
		assert.Equal(t, byte(0), finalResult[i])
	}
	for i := 250; i < 1024; i++ {
		assert.Equal(t, byte(0), finalResult[i])
	}
}

func TestCBTAwareReader_EOF(t *testing.T) {
	deviceData := make([]byte, 512)
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 256},
	}

	reader := NewCBTAwareReader(device, ranges, 512)

	// Read all data
	result := make([]byte, 512)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 512, n)

	// Try to read more - should get EOF
	extraBuf := make([]byte, 10)
	n, err = reader.Read(extraBuf)
	assert.Equal(t, 0, n)
	assert.Equal(t, io.EOF, err)
}

func TestCBTAwareReader_EmptyRanges(t *testing.T) {
	// Test with no changed ranges - entire device should be zero-filled
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{} // No changed ranges

	reader := NewCBTAwareReader(device, ranges, 1024)

	// Read all data
	result := make([]byte, 1024)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	// Everything should be zeros
	for i := 0; i < 1024; i++ {
		assert.Equal(t, byte(0), result[i], "byte %d should be zero", i)
	}
}

func TestCBTAwareReader_RangeAtStart(t *testing.T) {
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 100}, // Range at very start
	}

	reader := NewCBTAwareReader(device, ranges, 1024)

	result := make([]byte, 1024)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	// First 100 bytes should match device data
	assert.Equal(t, deviceData[0:100], result[0:100])

	// Rest should be zeros
	for i := 100; i < 1024; i++ {
		assert.Equal(t, byte(0), result[i])
	}
}

func TestCBTAwareReader_RangeAtEnd(t *testing.T) {
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 924, Length: 100}, // Range at very end
	}

	reader := NewCBTAwareReader(device, ranges, 1024)

	result := make([]byte, 1024)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	// First 924 bytes should be zeros
	for i := 0; i < 924; i++ {
		assert.Equal(t, byte(0), result[i])
	}

	// Last 100 bytes should match device data
	assert.Equal(t, deviceData[924:1024], result[924:1024])
}

func TestCBTAwareReader_PartialRead(t *testing.T) {
	// Test when read buffer is larger than remaining data
	deviceData := make([]byte, 100)
	for i := range deviceData {
		deviceData[i] = byte(i)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 50},
	}

	reader := NewCBTAwareReader(device, ranges, 100)

	// First read: will get the changed range (50 bytes)
	buf1 := make([]byte, 80)
	n, err := reader.Read(buf1)
	require.NoError(t, err)
	assert.Equal(t, 50, n) // Returns just the range

	// Second read: will get zeros for the gap (50 bytes)
	buf2 := make([]byte, 100)
	n, err = reader.Read(buf2)
	require.NoError(t, err)
	assert.Equal(t, 50, n) // Returns the remaining 50 bytes of zeros

	// Next read should return EOF
	buf3 := make([]byte, 10)
	n, err = reader.Read(buf3)
	assert.Equal(t, 0, n)
	assert.Equal(t, io.EOF, err)
}

func TestCBTAwareReader_Close(t *testing.T) {
	deviceData := make([]byte, 100)
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 50},
	}

	reader := NewCBTAwareReader(device, ranges, 100)

	// Close should succeed
	err := reader.Close()
	assert.NoError(t, err)

	// Reading after close should fail (file closed)
	buf := make([]byte, 10)
	_, err = reader.Read(buf)
	assert.Error(t, err)
}

func TestCBTAwareReader_CloseNilDevice(t *testing.T) {
	// Test Close with nil device (edge case)
	reader := &CBTAwareReader{
		device:        nil,
		changedRanges: []csi.ByteRange{},
		totalSize:     0,
	}

	err := reader.Close()
	assert.NoError(t, err)
}

func TestCBTAwareReader_LargeGap(t *testing.T) {
	// Test with large gap between ranges (stress test zero-filling)
	deviceData := make([]byte, 10*1024*1024) // 10MB
	// Only first and last KB have data
	for i := 0; i < 1024; i++ {
		deviceData[i] = byte(i % 256)
		deviceData[len(deviceData)-1024+i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 1024},
		{Offset: int64(len(deviceData) - 1024), Length: 1024},
	}

	reader := NewCBTAwareReader(device, ranges, int64(len(deviceData)))

	// Read in 1MB chunks to verify zero-filling efficiency
	chunkSize := 1024 * 1024
	buf := make([]byte, chunkSize)
	totalRead := 0

	for totalRead < len(deviceData) {
		n, err := reader.Read(buf)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		totalRead += n

		// For chunks in the middle gap, verify they're all zeros
		if totalRead > 1024 && totalRead < len(deviceData)-1024 {
			for i := 0; i < n; i++ {
				if buf[i] != 0 {
					t.Fatalf("Expected zero at position %d in gap region", totalRead-n+i)
				}
			}
		}
	}

	assert.Equal(t, len(deviceData), totalRead)
}

func TestCBTAwareReader_AdjacentRanges(t *testing.T) {
	// Test with adjacent ranges (no gap between them)
	deviceData := make([]byte, 1024)
	for i := range deviceData {
		deviceData[i] = byte(i % 256)
	}
	device := createMockBlockDevice(t, deviceData)

	ranges := []csi.ByteRange{
		{Offset: 0, Length: 256},
		{Offset: 256, Length: 256},
		{Offset: 512, Length: 256},
	}

	reader := NewCBTAwareReader(device, ranges, 1024)

	result := make([]byte, 1024)
	n, err := io.ReadFull(reader, result)
	require.NoError(t, err)
	assert.Equal(t, 1024, n)

	// First 768 bytes should match device (3 adjacent ranges)
	assert.Equal(t, deviceData[0:768], result[0:768])

	// Rest should be zeros
	for i := 768; i < 1024; i++ {
		assert.Equal(t, byte(0), result[i])
	}
}

func TestMin(t *testing.T) {
	tests := []struct {
		name     string
		a        int64
		b        int64
		expected int64
	}{
		{
			name:     "a less than b",
			a:        5,
			b:        10,
			expected: 5,
		},
		{
			name:     "b less than a",
			a:        10,
			b:        5,
			expected: 5,
		},
		{
			name:     "a equals b",
			a:        7,
			b:        7,
			expected: 7,
		},
		{
			name:     "zero values",
			a:        0,
			b:        0,
			expected: 0,
		},
		{
			name:     "negative values",
			a:        -5,
			b:        -10,
			expected: -10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := min(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}
