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

//nolint:unparam
package restic

const (
	// AWS specific environment variable
	awsProfileEnvVar         = "AWS_PROFILE"
	awsProfileKey            = "profile"
	awsCredentialsFileEnvVar = "AWS_SHARED_CREDENTIALS_FILE" //nolint:gosec
)

// getS3ResticEnvVars gets the environment variables that restic
// relies on (AWS_PROFILE) based on info in the provided object
// storage location config map.
func getS3ResticEnvVars(config map[string]string) (map[string]string, error) {
	result := make(map[string]string)

	if credentialsFile, ok := config[credentialsFileKey]; ok {
		result[awsCredentialsFileEnvVar] = credentialsFile
	}

	if profile, ok := config[awsProfileKey]; ok {
		result[awsProfileEnvVar] = profile
	}

	return result, nil
}
