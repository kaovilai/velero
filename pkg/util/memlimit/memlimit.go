/*
Copyright the Velero contributors.

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

// Package memlimit derives the Go runtime soft memory limit from the
// container memory limit.
package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
)

const (
	// limitPercent is the share of the container memory limit used as the Go
	// soft memory limit. The rest is headroom for memory outside the Go heap
	// and for the time the GC needs to react once the soft limit is reached.
	limitPercent = 80

	// cgroup v1 reports a value close to MaxInt64 when no limit is set.
	cgroupV1Unlimited = int64(1) << 62
)

var (
	cgroupRoot     = "/sys/fs/cgroup"
	setMemoryLimit = debug.SetMemoryLimit
)

// SetFromContainerLimit sets the Go soft memory limit to a share of the
// container memory limit, so the GC collects more aggressively before the
// container gets OOM killed. Without it the heap may grow to twice the live
// data (GOGC=100) regardless of the limit. It does nothing if GOMEMLIMIT is
// set or if the container has no memory limit.
func SetFromContainerLimit(logger logrus.FieldLogger) {
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		logger.Infof("GOMEMLIMIT is set to %s, keep it", v)
		return
	}

	limit, err := containerMemoryLimit(cgroupRoot)
	if err != nil {
		logger.WithError(err).Warn("Failed to read container memory limit, Go memory limit is not set")
		return
	}

	if limit <= 0 {
		logger.Info("No container memory limit, Go memory limit is not set")
		return
	}

	goLimit := limit / 100 * limitPercent
	setMemoryLimit(goLimit)

	logger.Infof("Set Go memory limit to %d bytes (%d%% of container memory limit %d bytes)", goLimit, limitPercent, limit)
}

// containerMemoryLimit returns the memory limit of the cgroup mounted at root,
// or 0 if there is no limit.
func containerMemoryLimit(root string) (int64, error) {
	for _, file := range []string{
		filepath.Join(root, "memory.max"),                      // cgroup v2
		filepath.Join(root, "memory", "memory.limit_in_bytes"), // cgroup v1
	} {
		data, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, errors.Wrapf(err, "error reading %s", file)
		}

		return parseMemoryLimit(data)
	}

	return 0, nil
}

func parseMemoryLimit(data []byte) (int64, error) {
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return 0, nil
	}

	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.Wrapf(err, "invalid memory limit %q", value)
	}

	if limit >= cgroupV1Unlimited {
		return 0, nil
	}

	return limit, nil
}
