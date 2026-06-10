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

package server

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

type TestBase struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

type testSpec struct {
	Name    string `json:"name"`
	Count   int    `json:"count,omitempty"`
	Ignored string `json:"-"`
	private string //nolint:unused
}

type testSpecWithEmbedded struct {
	TestBase
	Extra string `json:"extra"`
}

func TestJsonFieldNames(t *testing.T) {
	tests := []struct {
		name     string
		input    reflect.Type
		expected []string
	}{
		{
			name:     "simple struct",
			input:    reflect.TypeFor[testSpec](),
			expected: []string{"name", "count"},
		},
		{
			name:     "pointer to struct",
			input:    reflect.TypeFor[*testSpec](),
			expected: []string{"name", "count"},
		},
		{
			name:     "struct with anonymous embedded",
			input:    reflect.TypeFor[testSpecWithEmbedded](),
			expected: []string{"name", "count", "extra"},
		},
		{
			name:     "nil type",
			input:    nil,
			expected: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := jsonFieldNames(tc.input)
			if tc.expected == nil {
				assert.Nil(t, result)
				return
			}
			for _, field := range tc.expected {
				assert.True(t, result.Has(field), "expected field %q", field)
			}
			assert.False(t, result.Has("Ignored"))
			assert.False(t, result.Has("private"))
		})
	}
}

func TestSchemaPropertyNames(t *testing.T) {
	schema := &apiextv1.JSONSchemaProps{
		Properties: map[string]apiextv1.JSONSchemaProps{
			"spec": {
				Properties: map[string]apiextv1.JSONSchemaProps{
					"name":  {Type: "string"},
					"count": {Type: "integer"},
				},
			},
			"status": {
				Properties: map[string]apiextv1.JSONSchemaProps{
					"phase":   {Type: "string"},
					"message": {Type: "string"},
				},
			},
		},
	}

	t.Run("extract spec properties", func(t *testing.T) {
		result := schemaPropertyNames(schema, "spec")
		require.NotNil(t, result)
		assert.True(t, result.Has("name"))
		assert.True(t, result.Has("count"))
		assert.Equal(t, 2, result.Len())
	})

	t.Run("extract status properties", func(t *testing.T) {
		result := schemaPropertyNames(schema, "status")
		require.NotNil(t, result)
		assert.True(t, result.Has("phase"))
		assert.True(t, result.Has("message"))
	})

	t.Run("nonexistent path", func(t *testing.T) {
		result := schemaPropertyNames(schema, "nonexistent")
		assert.Nil(t, result)
	})

	t.Run("nil schema", func(t *testing.T) {
		result := schemaPropertyNames(nil, "spec")
		assert.Nil(t, result)
	})
}

func TestCheckMissing(t *testing.T) {
	schema := &apiextv1.JSONSchemaProps{
		Properties: map[string]apiextv1.JSONSchemaProps{
			"spec": {
				Properties: map[string]apiextv1.JSONSchemaProps{
					"name": {Type: "string"},
				},
			},
		},
	}

	t.Run("missing field detected", func(t *testing.T) {
		missing := checkMissing(reflect.TypeFor[testSpec](), schema, "spec", "tests.velero.io")
		assert.Len(t, missing, 1)
		assert.Contains(t, missing[0], "count")
	})

	t.Run("no missing fields", func(t *testing.T) {
		fullSchema := &apiextv1.JSONSchemaProps{
			Properties: map[string]apiextv1.JSONSchemaProps{
				"spec": {
					Properties: map[string]apiextv1.JSONSchemaProps{
						"name":  {Type: "string"},
						"count": {Type: "integer"},
					},
				},
			},
		}
		missing := checkMissing(reflect.TypeFor[testSpec](), fullSchema, "spec", "tests.velero.io")
		assert.Empty(t, missing)
	})

	t.Run("extra fields in CRD are OK", func(t *testing.T) {
		extraSchema := &apiextv1.JSONSchemaProps{
			Properties: map[string]apiextv1.JSONSchemaProps{
				"spec": {
					Properties: map[string]apiextv1.JSONSchemaProps{
						"name":      {Type: "string"},
						"count":     {Type: "integer"},
						"newField":  {Type: "string"},
						"anotherEx": {Type: "boolean"},
					},
				},
			},
		}
		missing := checkMissing(reflect.TypeFor[testSpec](), extraSchema, "spec", "tests.velero.io")
		assert.Empty(t, missing)
	})

	t.Run("nil go type", func(t *testing.T) {
		missing := checkMissing(nil, schema, "spec", "tests.velero.io")
		assert.Nil(t, missing)
	})
}

func TestExpectedCRDSchemas(t *testing.T) {
	expectations := expectedCRDSchemas()
	assert.NotEmpty(t, expectations)

	crdNames := make(map[string]bool)
	for _, exp := range expectations {
		crdNames[exp.crdName] = true
		assert.NotEmpty(t, exp.crdName, "CRD name should not be empty")
		assert.NotEmpty(t, exp.apiGroupVersion, "API group version should not be empty")
	}

	assert.True(t, crdNames["backups.velero.io"], "should include backups CRD")
	assert.True(t, crdNames["restores.velero.io"], "should include restores CRD")
	assert.True(t, crdNames["schedules.velero.io"], "should include schedules CRD")
	assert.True(t, crdNames["datauploads.velero.io"], "should include datauploads CRD")
	assert.True(t, crdNames["datadownloads.velero.io"], "should include datadownloads CRD")
}
