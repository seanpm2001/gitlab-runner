//go:build !integration

package common

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheCheckPolicy(t *testing.T) {
	for num, tc := range []struct {
		object      CachePolicy
		subject     CachePolicy
		expected    bool
		expectErr   bool
		description string
	}{
		{CachePolicyPullPush, CachePolicyPull, true, false, "pull-push allows pull"},
		{CachePolicyPullPush, CachePolicyPush, true, false, "pull-push allows push"},
		{CachePolicyUndefined, CachePolicyPull, true, false, "undefined allows pull"},
		{CachePolicyUndefined, CachePolicyPush, true, false, "undefined allows push"},
		{CachePolicyPull, CachePolicyPull, true, false, "pull allows pull"},
		{CachePolicyPull, CachePolicyPush, false, false, "pull forbids push"},
		{CachePolicyPush, CachePolicyPull, false, false, "push forbids pull"},
		{CachePolicyPush, CachePolicyPush, true, false, "push allows push"},
		{"unknown", CachePolicyPull, false, true, "unknown raises error on pull"},
		{"unknown", CachePolicyPush, false, true, "unknown raises error on push"},
	} {
		cache := Cache{Policy: tc.object}

		result, err := cache.CheckPolicy(tc.subject)
		if tc.expectErr {
			assert.Errorf(t, err, "case %d: %s", num, tc.description)
		} else {
			assert.NoErrorf(t, err, "case %d: %s", num, tc.description)
		}

		assert.Equal(t, tc.expected, result, "case %d: %s", num, tc.description)
	}
}

func TestShouldCache(t *testing.T) {
	for _, params := range []struct {
		jobSuccess          bool
		when                CacheWhen
		expectedShouldCache bool
	}{
		{true, CacheWhenOnSuccess, true},
		{true, CacheWhenAlways, true},
		{true, CacheWhenOnFailure, false},
		{false, CacheWhenOnSuccess, false},
		{false, CacheWhenAlways, true},
		{false, CacheWhenOnFailure, true},
	} {
		tn := "jobSuccess=" + strconv.FormatBool(params.jobSuccess) + ",when=" + string(params.when)

		t.Run(tn, func(t *testing.T) {
			expected := params.expectedShouldCache

			actual := params.when.ShouldCache(params.jobSuccess)

			assert.Equal(
				t,
				actual,
				expected,
				"Value returned from ShouldCache was not as expected",
			)
		})
	}
}

func TestSecrets_expandVariables(t *testing.T) {
	testServerURL := "server-url"
	testNamespace := "custom-namespace"
	testAuthName := "auth-name"
	testAuthPath := "auth-path"
	testAuthJWT := "auth-jwt"
	testAuthRole := "auth-role"
	testAuthUnknown := "auth-unknown"
	testEngineName := "engine-name"
	testEnginePath := "engine-path"
	testPath := "secret-path"
	testField := "secret-field"

	variables := JobVariables{
		{Key: "CI_VAULT_SERVER_URL", Value: testServerURL},
		{Key: "CI_VAULT_NAMESPACE", Value: testNamespace},
		{Key: "CI_VAULT_AUTH_NAME", Value: testAuthName},
		{Key: "CI_VAULT_AUTH_PATH", Value: testAuthPath},
		{Key: "CI_VAULT_AUTH_JWT", Value: testAuthJWT},
		{Key: "CI_VAULT_AUTH_ROLE", Value: testAuthRole},
		{Key: "CI_VAULT_AUTH_UNKNOWN_DATA", Value: testAuthUnknown},
		{Key: "CI_VAULT_ENGINE_NAME", Value: testEngineName},
		{Key: "CI_VAULT_ENGINE_PATH", Value: testEnginePath},
		{Key: "CI_VAULT_PATH", Value: testPath},
		{Key: "CI_VAULT_FIELD", Value: testField},
	}

	assertValue := func(t *testing.T, prefix string, variableValue string, testedValue interface{}) {
		assert.Equal(
			t,
			fmt.Sprintf("%s %s", prefix, variableValue),
			testedValue,
		)
	}

	tests := map[string]struct {
		secrets       Secrets
		assertSecrets func(t *testing.T, secrets Secrets)
	}{
		"no secrets defined": {
			secrets: nil,
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.Nil(t, secrets)
			},
		},
		"nil vault secret": {
			secrets: Secrets{
				"VAULT": Secret{
					Vault: nil,
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.Nil(t, secrets["VAULT"].Vault)
			},
		},
		"vault missing data": {
			secrets: Secrets{
				"VAULT": Secret{
					Vault: &VaultSecret{},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.NotNil(t, secrets["VAULT"].Vault)
			},
		},
		"vault missing jwt data": {
			secrets: Secrets{
				"VAULT": Secret{
					Vault: &VaultSecret{
						Server: VaultServer{
							Auth: VaultAuth{
								Data: map[string]interface{}{
									"role": testAuthRole,
								},
							},
						},
					},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				require.NotNil(t, secrets["VAULT"].Vault)
				assert.Equal(t, testAuthRole, secrets["VAULT"].Vault.Server.Auth.Data["role"])
			},
		},
		"vault secret defined": {
			secrets: Secrets{
				"VAULT": Secret{
					Vault: &VaultSecret{
						Server: VaultServer{
							URL:       "url ${CI_VAULT_SERVER_URL}",
							Namespace: "namespace ${CI_VAULT_NAMESPACE}",
							Auth: VaultAuth{
								Name: "name ${CI_VAULT_AUTH_NAME}",
								Path: "path ${CI_VAULT_AUTH_PATH}",
								Data: map[string]interface{}{
									"jwt":     "jwt ${CI_VAULT_AUTH_JWT}",
									"role":    "role ${CI_VAULT_AUTH_ROLE}",
									"unknown": "unknown ${CI_VAULT_AUTH_UNKNOWN_DATA}",
								},
							},
						},
						Engine: VaultEngine{
							Name: "name ${CI_VAULT_ENGINE_NAME}",
							Path: "path ${CI_VAULT_ENGINE_PATH}",
						},
						Path:  "path ${CI_VAULT_PATH}",
						Field: "field ${CI_VAULT_FIELD}",
					},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				require.NotNil(t, secrets["VAULT"].Vault)
				assertValue(t, "url", testServerURL, secrets["VAULT"].Vault.Server.URL)
				assertValue(t, "namespace", testNamespace, secrets["VAULT"].Vault.Server.Namespace)
				assertValue(t, "name", testAuthName, secrets["VAULT"].Vault.Server.Auth.Name)
				assertValue(t, "path", testAuthPath, secrets["VAULT"].Vault.Server.Auth.Path)
				require.NotNil(t, secrets["VAULT"].Vault.Server.Auth.Data["jwt"])
				assertValue(t, "jwt", testAuthJWT, secrets["VAULT"].Vault.Server.Auth.Data["jwt"])
				require.NotNil(t, secrets["VAULT"].Vault.Server.Auth.Data["role"])
				assertValue(t, "role", testAuthRole, secrets["VAULT"].Vault.Server.Auth.Data["role"])
				require.NotNil(t, secrets["VAULT"].Vault.Server.Auth.Data["unknown"])
				assertValue(t, "unknown", testAuthUnknown, secrets["VAULT"].Vault.Server.Auth.Data["unknown"])
				assertValue(t, "name", testEngineName, secrets["VAULT"].Vault.Engine.Name)
				assertValue(t, "path", testEnginePath, secrets["VAULT"].Vault.Engine.Path)
				assertValue(t, "path", testPath, secrets["VAULT"].Vault.Path)
				assertValue(t, "field", testField, secrets["VAULT"].Vault.Field)
			},
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			assert.NotPanics(t, func() {
				tt.secrets.expandVariables(variables)
				tt.assertSecrets(t, tt.secrets)
			})
		})
	}
}

func TestAzureKeyVaultSecrets_expandVariables(t *testing.T) {
	testName := "key-name"
	testVersion := "key-version"
	testAuthJWT := "auth-jwt"

	variables := JobVariables{
		{Key: "CI_AZURE_KEY_VAULT_KEY_NAME", Value: testName},
		{Key: "CI_AZURE_KEY_VAULT_KEY_VERSION", Value: testVersion},
		{Key: "CI_AZURE_KEY_VAULT_AUTH_JWT", Value: testAuthJWT},
	}

	assertValue := func(t *testing.T, prefix string, variableValue string, testedValue interface{}) {
		assert.Equal(
			t,
			fmt.Sprintf("%s %s", prefix, variableValue),
			testedValue,
		)
	}

	tests := map[string]struct {
		secrets       Secrets
		assertSecrets func(t *testing.T, secrets Secrets)
	}{
		"no secrets defined": {
			secrets: nil,
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.Nil(t, secrets)
			},
		},
		"nil vault secret": {
			secrets: Secrets{
				"VAULT": Secret{
					AzureKeyVault: nil,
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.Nil(t, secrets["VAULT"].Vault)
			},
		},
		"vault missing data": {
			secrets: Secrets{
				"VAULT": Secret{
					AzureKeyVault: &AzureKeyVaultSecret{},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				assert.NotNil(t, secrets["VAULT"].AzureKeyVault)
			},
		},
		"vault missing jwt data": {
			secrets: Secrets{
				"VAULT": Secret{
					AzureKeyVault: &AzureKeyVaultSecret{
						Name:    testName,
						Version: testVersion,
						Server: AzureKeyVaultServer{
							ClientID: "test_client_id",
							TenantID: "test_tenant_id",
							URL:      "test_url",
						},
					},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				require.NotNil(t, secrets["VAULT"].AzureKeyVault)
				assert.Equal(t, testName, secrets["VAULT"].AzureKeyVault.Name)
				assert.Equal(t, testVersion, secrets["VAULT"].AzureKeyVault.Version)
			},
		},
		"vault secret defined": {
			secrets: Secrets{
				"VAULT": Secret{
					AzureKeyVault: &AzureKeyVaultSecret{
						Name:    "name ${CI_AZURE_KEY_VAULT_KEY_NAME}",
						Version: "version ${CI_AZURE_KEY_VAULT_KEY_VERSION}",
						Server: AzureKeyVaultServer{
							ClientID: "client_id",
							TenantID: "tenant_id",
							JWT:      "jwt ${CI_AZURE_KEY_VAULT_AUTH_JWT}",
							URL:      "url",
						},
					},
				},
			},
			assertSecrets: func(t *testing.T, secrets Secrets) {
				require.NotNil(t, secrets["VAULT"].AzureKeyVault)
				assertValue(t, "name", testName, secrets["VAULT"].AzureKeyVault.Name)
				assertValue(t, "version", testVersion, secrets["VAULT"].AzureKeyVault.Version)
				assertValue(t, "jwt", testAuthJWT, secrets["VAULT"].AzureKeyVault.Server.JWT)
			},
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			assert.NotPanics(t, func() {
				tt.secrets.expandVariables(variables)
				tt.assertSecrets(t, tt.secrets)
			})
		})
	}
}

func TestJobResponse_JobURL(t *testing.T) {
	jobID := int64(1)

	testCases := map[string]string{
		"http://user:pass@gitlab.example.com/my-namespace/my-project.git":     "http://gitlab.example.com/my-namespace/my-project/-/jobs/1",
		"http://user:pass@gitlab.example.com/my-namespace/my-project":         "http://gitlab.example.com/my-namespace/my-project/-/jobs/1",
		"http://user:pass@gitlab.example.com/my-namespace/my.git.project.git": "http://gitlab.example.com/my-namespace/my.git.project/-/jobs/1",
		"http://user:pass@gitlab.example.com/my-namespace/my.git.project":     "http://gitlab.example.com/my-namespace/my.git.project/-/jobs/1",
	}

	for repoURL, expectedURL := range testCases {
		job := JobResponse{
			ID: jobID,
			GitInfo: GitInfo{
				RepoURL: repoURL,
			},
		}

		assert.Equal(t, expectedURL, job.JobURL())
	}
}

func Test_Image_ExecutorOptions_UnmarshalJSON(t *testing.T) {
	tests := map[string]struct {
		json           string
		expected       func(*testing.T, Image)
		expectedErrMsg []string
	}{
		"empty image": {
			json: "{}",
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, no docker": {
			json: `{"executor_opts":{}}`,
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "", i.ExecutorOptions.Docker.Platform)
			},
		},
		"docker, no platform": {
			json: `{"executor_opts":{"docker": {}}}`,
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "", i.ExecutorOptions.Docker.Platform)
			},
		},
		"docker, with platform": {
			json: `{"executor_opts":{"docker": {"platform": "amd64"}}}`,
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "amd64", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, docker, invalid executor": {
			json:           `{"executor_opts":{"k8s": {}, "docker": {"platform": "amd64"}}}`,
			expectedErrMsg: []string{`Unsupported "image" options [k8s] for "executor_opts"; supported options are [docker]`},
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "amd64", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, no docker, invalid executor": {
			json:           `{"executor_opts":{"k8s": {}}}`,
			expectedErrMsg: []string{`Unsupported "image" options [k8s] for "executor_opts"; supported options are [docker]`},
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, docker, no platform, invalid property": {
			json:           `{"executor_opts":{"docker": {"foobar": 1234}}}`,
			expectedErrMsg: []string{`Unsupported "image" options [foobar] for "docker executor"; supported options are [platform]`},
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, docker, platform, invalid property": {
			json:           `{"executor_opts":{"docker": {"platform": "amd64", "foobar": 1234}}}`,
			expectedErrMsg: []string{`Unsupported "image" options [foobar] for "docker executor"; supported options are [platform]`},
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "amd64", i.ExecutorOptions.Docker.Platform)
			},
		},
		"executor_opts, invalid executor, docker, platform, invalid property": {
			json: `{"executor_opts":{"k8s": {}, "docker": {"platform": "amd64", "foobar": 1234}}}`,
			expectedErrMsg: []string{
				`Unsupported "image" options [k8s] for "executor_opts"; supported options are [docker]`,
				`Unsupported "image" options [foobar] for "docker executor"; supported options are [platform]`,
			},
			expected: func(t *testing.T, i Image) {
				assert.Equal(t, "amd64", i.ExecutorOptions.Docker.Platform)
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := Image{}
			err := json.Unmarshal([]byte(tt.json), &got)
			assert.NoError(t, err)
			tt.expected(t, got)

			if len(tt.expectedErrMsg) == 0 {
				assert.Nil(t, got.UnsupportedOptions())
			} else {
				for i := range tt.expectedErrMsg {
					assert.Contains(t, got.UnsupportedOptions().Error(), tt.expectedErrMsg[i])
				}
			}
		})
	}
}
