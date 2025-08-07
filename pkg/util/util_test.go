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

package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContains(t *testing.T) {
	testCases := []struct {
		name           string
		inSlice        []string
		inKey          string
		expectedResult bool
	}{
		{
			name:           "should find the key",
			inSlice:        []string{"key1", "key2", "key3", "key4", "key5"},
			inKey:          "key3",
			expectedResult: true,
		},
		{
			name:           "should not find the key in non-empty slice",
			inSlice:        []string{"key1", "key2", "key3", "key4", "key5"},
			inKey:          "key300",
			expectedResult: false,
		},
		{
			name:           "should not find key in empty slice",
			inSlice:        []string{},
			inKey:          "key300",
			expectedResult: false,
		},
		{
			name:           "should not find key in nil slice",
			inSlice:        nil,
			inKey:          "key300",
			expectedResult: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualResult := Contains(tc.inSlice, tc.inKey)
			assert.Equal(t, tc.expectedResult, actualResult)
		})
	}
}

func TestSanitizeBackupStorageLocationError(t *testing.T) {
	testCases := []struct {
		name           string
		inputError     error
		expectedResult string
	}{
		{
			name:           "nil error",
			inputError:     nil,
			expectedResult: "",
		},
		{
			name:           "Azure container not found with HTTP details",
			inputError:     errors.New("azure storage error: status code: 404 response body: <Error><Code>ContainerNotFound</Code><Message>The specified container does not exist. RequestId:abc-123-def Time:2023-01-01T10:00:00.000Z</Message></Error>"),
			expectedResult: "container does not exist",
		},
		{
			name:           "Azure container not found simple",
			inputError:     errors.New("container not found"),
			expectedResult: "container not found",
		},
		{
			name:           "S3 bucket does not exist",
			inputError:     errors.New("bucket does not exist"),
			expectedResult: "bucket does not exist",
		},
		{
			name:           "Access denied error",
			inputError:     errors.New("access denied: insufficient permissions"),
			expectedResult: "access denied",
		},
		{
			name:           "Unauthorized error",
			inputError:     errors.New("unauthorized access to resource"),
			expectedResult: "unauthorized access",
		},
		{
			name:           "Invalid credentials",
			inputError:     errors.New("invalid credentials provided"),
			expectedResult: "invalid credentials",
		},
		{
			name:           "Complex Azure error with multiple patterns",
			inputError:     errors.New("azure storage error: status code: 404 requestid:abc-123-def time:2023-01-01T10:00:00.000Z response body: <Error><Code>ContainerNotFound</Code></Error> container does not exist"),
			expectedResult: "container does not exist",
		},
		{
			name:           "Generic error message",
			inputError:     errors.New("network timeout occurred"),
			expectedResult: "network timeout occurred",
		},
		{
			name:           "Empty error message after sanitization",
			inputError:     errors.New("status code: 500 requestid:xyz time:2023-01-01T10:00:00.000Z"),
			expectedResult: "unknown error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := SanitizeBackupStorageLocationError(tc.inputError)
			assert.Equal(t, tc.expectedResult, result)
		})
	}
}
