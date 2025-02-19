//go:build integration

package autoscaler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildtest"
	"gitlab.com/gitlab-org/gitlab-runner/helpers"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/ssh"
	"gitlab.com/gitlab-org/gitlab-runner/shells/shellstest"
)

func newRunnerConfig(t *testing.T, shell string) *common.RunnerConfig {
	helpers.SkipIntegrationTests(t, "fleeting-plugin-static", "--version")

	// In theory, pwsh should work if getImage() is upgraded to use the alpine powershell image,
	// however, in practice, we get errors in CI with the pwsh helper image selected.
	// TODO: fix this for pwsh when using pwsh helper image
	if shell == "pwsh" {
		t.Skip()
	}

	if shell == "cmd" || shell == "powershell" {
		t.Skip()
	}

	dir := t.TempDir()

	t.Log("Build directory:", dir)

	srv, err := ssh.NewStubServer("root", "password")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, srv.Stop())
	})

	srv.ExecuteLocal = true

	image := getImage()

	return &common.RunnerConfig{
		SystemIDState: common.NewSystemIDState(),
		RunnerCredentials: common.RunnerCredentials{
			Token: "runner-token",
		},
		RunnerSettings: common.RunnerSettings{
			BuildsDir: dir,
			Executor:  "docker-autoscaler",
			Shell:     shell,
			Cache:     &common.CacheConfig{},
			Docker: &common.DockerConfig{
				Image: image,
				// TODO: This is a temporary fix! This test should be able to use the helper image built in the same pipeline as
				// the test job.
				HelperImage: "gitlab/gitlab-runner-helper:x86_64-latest",
			},
			Autoscaler: &common.AutoscalerConfig{
				MaxUseCount:         1,
				CapacityPerInstance: 1,
				MaxInstances:        1,
				Plugin:              "fleeting-plugin-static",
				PluginConfig: common.AutoscalerSettingsMap{
					"instances": map[string]map[string]string{
						"local": {
							"username":      srv.User,
							"password":      srv.Password,
							"timeout":       "1m",
							"external_addr": srv.Host() + ":" + srv.Port(),
							"internal_addr": srv.Host() + ":" + srv.Port(),
						},
					},
				},
			},
		},
	}
}

func setupAcquireBuild(t *testing.T, build *common.Build) {
	provider := common.GetExecutorProvider("docker-autoscaler")
	data, err := provider.Acquire(build.Runner)
	require.NoError(t, err)

	build.ExecutorData = data
	t.Cleanup(func() {
		provider.Release(build.Runner, build.ExecutorData)

		if shutdownable, ok := provider.(common.ManagedExecutorProvider); ok {
			shutdownable.Shutdown(context.Background())
		}
	})
}

func TestBuildSuccess(t *testing.T) {
	shellstest.OnEachShell(t, func(t *testing.T, shell string) {
		successfulBuild, err := common.GetRemoteSuccessfulBuild()
		require.NoError(t, err)

		build := &common.Build{
			JobResponse: successfulBuild,
			Runner:      newRunnerConfig(t, shell),
		}
		setupAcquireBuild(t, build)

		require.NoError(t, buildtest.RunBuild(t, build))
	})
}

func TestBuildCancel(t *testing.T) {
	shellstest.OnEachShell(t, func(t *testing.T, shell string) {
		buildtest.RunBuildWithCancel(t, newRunnerConfig(t, shell), setupAcquireBuild)
	})
}

func TestBuildMasking(t *testing.T) {
	shellstest.OnEachShell(t, func(t *testing.T, shell string) {
		buildtest.RunBuildWithMasking(t, newRunnerConfig(t, shell), setupAcquireBuild)
	})
}

func TestBuildExpandedFileVariable(t *testing.T) {
	shellstest.OnEachShell(t, func(t *testing.T, shell string) {
		buildtest.RunBuildWithExpandedFileVariable(t, newRunnerConfig(t, shell), setupAcquireBuild)
	})
}
