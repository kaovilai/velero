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

package restic

const (
	// GCP specific environment variable
	gcpCredentialsFileEnvVar = "GOOGLE_APPLICATION_CREDENTIALS" //nolint:gosec
)

// getGCPResticEnvVars gets the environment variables that restic relies
// on based on info in the provided object storage location config map.
func getGCPResticEnvVars(config map[string]string) (map[string]string, error) { //nolint:unparam
	result := make(map[string]string)

	if credentialsFile, ok := config[credentialsFileKey]; ok {
		result[gcpCredentialsFileEnvVar] = credentialsFile
	}

	return result, nil
}
