package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/exec"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/labels"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/networks"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/pull"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/volumes"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/volumes/parser"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/volumes/permission"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker/internal/wait"
	"gitlab.com/gitlab-org/gitlab-runner/helpers"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/container/helperimage"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/docker"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/featureflags"
	"gitlab.com/gitlab-org/gitlab-runner/helpers/limitwriter"
	"gitlab.com/gitlab-org/gitlab-runner/shells"
)

const (
	ExecutorStagePrepare common.ExecutorStage = "docker_prepare"
	ExecutorStageRun     common.ExecutorStage = "docker_run"
	ExecutorStageCleanup common.ExecutorStage = "docker_cleanup"

	ExecutorStageCreatingBuildVolumes common.ExecutorStage = "docker_creating_build_volumes"
	ExecutorStageCreatingServices     common.ExecutorStage = "docker_creating_services"
	ExecutorStageCreatingUserVolumes  common.ExecutorStage = "docker_creating_user_volumes"
	ExecutorStagePullingImage         common.ExecutorStage = "docker_pulling_image"
)

const ServiceLogOutputLimit = 64 * 1024

var useInit = true

var PrebuiltImagesPaths []string

const (
	labelServiceType = "service"
	labelWaitType    = "wait"
)

// internalFakeTunnelHostname is an internal hostname we provide the Docker client
// when we provide a tunnelled dialer implementation. Because we're overriding
// the dialer, this domain should never be used by the client, but we use the
// reserved TLD ".invalid" for safety.
const internalFakeTunnelHostname = "http://internel.tunnel.invalid"

var neverRestartPolicy = container.RestartPolicy{Name: "no"}

var (
	errVolumesManagerUndefined  = errors.New("volumesManager is undefined")
	errNetworksManagerUndefined = errors.New("networksManager is undefined")
)

type executor struct {
	executors.AbstractExecutor
	client                    docker.Client
	volumeParser              parser.Parser
	newVolumePermissionSetter func() (permission.Setter, error)
	info                      types.Info
	waiter                    wait.KillWaiter

	temporary        []string // IDs of containers that should be removed
	buildContainerID string

	services []*types.Container

	links []string

	devices        []container.DeviceMapping
	deviceRequests []container.DeviceRequest

	helperImageInfo helperimage.Info

	volumesManager  volumes.Manager
	networksManager networks.Manager
	labeler         labels.Labeler
	pullManager     pull.Manager

	networkMode container.NetworkMode

	projectUniqRandomizedName string

	tunnelClient executors.Client
}

func init() {
	runner, err := os.Executable()
	if err != nil {
		logrus.Errorln(
			"Docker executor: unable to detect gitlab-runner folder, "+
				"prebuilt image helpers will be loaded from remote registry.",
			err,
		)
	}

	runnerFolder := filepath.Dir(runner)

	PrebuiltImagesPaths = []string{
		// When gitlab-runner is running from repository root
		filepath.Join(runnerFolder, "out/helper-images"),
		// When gitlab-runner is running from `out/binaries`
		filepath.Join(runnerFolder, "../helper-images"),
		// Add working directory path, used when running from temp directory, such as with `go run`
		filepath.Join(helpers.GetCurrentWorkingDirectory(), "out/helper-images"),
	}
	if runtime.GOOS == "linux" {
		// This section covers the Linux packaged app scenario, with the binary in /usr/bin.
		// The helper images are located in /usr/lib/gitlab-runner/helper-images,
		// as part of the packaging done in the create_package function in ci/package
		PrebuiltImagesPaths = append(
			PrebuiltImagesPaths,
			filepath.Join(runnerFolder, "../lib/gitlab-runner/helper-images"),
		)
	}
}

func (e *executor) getServiceVariables(serviceDefinition common.Image) []string {
	variables := e.Build.GetAllVariables().PublicOrInternal()
	variables = append(variables, serviceDefinition.Variables...)

	return variables.Expand().StringList()
}

func (e *executor) expandAndGetDockerImage(
	imageName string,
	allowedImages []string,
	dockerOptions common.ImageDockerOptions,
	imagePullPolicies []common.DockerPullPolicy,
) (*types.ImageInspect, error) {
	imageName, err := e.expandImageName(imageName, allowedImages)
	if err != nil {
		return nil, err
	}

	image, err := e.pullManager.GetDockerImage(imageName, dockerOptions, imagePullPolicies)
	if err != nil {
		return nil, err
	}

	return image, nil
}

func (e *executor) loadPrebuiltImage(path, ref, tag string) (*types.ImageInspect, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}

		return nil, fmt.Errorf("cannot load prebuilt image: %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	e.Debugln("Loading prebuilt image...")

	source := types.ImageImportSource{
		Source:     file,
		SourceName: "-",
	}
	options := types.ImageImportOptions{
		Tag: tag,
		// NOTE: The ENTRYPOINT metadata is not preserved on export, so we need to reapply this metadata on import.
		// See https://gitlab.com/gitlab-org/gitlab-runner/-/merge_requests/2058#note_388341301
		Changes: []string{`ENTRYPOINT ["/usr/bin/dumb-init", "/entrypoint"]`},
	}

	if err = e.client.ImageImportBlocking(e.Context, source, ref, options); err != nil {
		return nil, fmt.Errorf("failed to import image: %w", err)
	}

	image, _, err := e.client.ImageInspectWithRaw(e.Context, ref+":"+tag)
	if err != nil {
		e.Debugln("Inspecting imported image", ref, "failed:", err)
		return nil, err
	}

	return &image, err
}

func (e *executor) getPrebuiltImage() (*types.ImageInspect, error) {
	if imageNameFromConfig := e.ExpandValue(e.Config.Docker.HelperImage); imageNameFromConfig != "" {
		e.Debugln(
			"Pull configured helper_image for predefined container instead of import bundled image",
			imageNameFromConfig,
			"...",
		)

		e.Println("Using helper image: ", imageNameFromConfig, " (overridden, default would be ", e.helperImageInfo, ")")

		return e.pullManager.GetDockerImage(imageNameFromConfig, common.ImageDockerOptions{}, nil)
	}

	e.Debugln(fmt.Sprintf("Looking for prebuilt image %s...", e.helperImageInfo))
	image, _, err := e.client.ImageInspectWithRaw(e.Context, e.helperImageInfo.String())
	if err == nil {
		return &image, nil
	}

	// Try to load prebuilt image from local filesystem
	loadedImage := e.getLocalHelperImage()
	if loadedImage != nil {
		return loadedImage, nil
	}

	e.Println("Using helper image: ", e.helperImageInfo.String())

	// Fall back to getting image from registry
	e.Debugln(fmt.Sprintf("Loading image form registry: %s", e.helperImageInfo))
	return e.pullManager.GetDockerImage(e.helperImageInfo.String(), common.ImageDockerOptions{}, nil)
}

func (e *executor) getLocalHelperImage() *types.ImageInspect {
	if !e.helperImageInfo.IsSupportingLocalImport {
		return nil
	}

	var flavor string
	if e.Config.Docker != nil {
		flavor = e.ExpandValue(e.Config.Docker.HelperImageFlavor)
	}

	architecture := e.helperImageInfo.Architecture
	prebuiltFileName := getPrebuiltFileName(architecture, flavor, e.Config.Shell)
	for _, dockerPrebuiltImagesPath := range PrebuiltImagesPaths {
		dockerPrebuiltImageFilePath := filepath.Join(dockerPrebuiltImagesPath, prebuiltFileName)
		image, err := e.loadPrebuiltImage(
			dockerPrebuiltImageFilePath,
			e.helperImageInfo.Name,
			e.helperImageInfo.Tag,
		)
		if err != nil {
			e.Debugln("Failed to load prebuilt image from:", dockerPrebuiltImageFilePath, "error:", err)
			continue
		}

		return image
	}

	return nil
}

func getPrebuiltFileName(architecture, flavor string, shell string) string {
	if flavor == "" {
		flavor = helperimage.DefaultFlavor
	}

	if shell == shells.SNPwsh {
		return fmt.Sprintf("prebuilt-%s-%s-%s%s", flavor, architecture, shell, prebuiltImageExtension)
	}

	return fmt.Sprintf("prebuilt-%s-%s%s", flavor, architecture, prebuiltImageExtension)
}

func (e *executor) getBuildImage() (*types.ImageInspect, error) {
	imageName, err := e.expandImageName(e.Build.Image.Name, []string{})
	if err != nil {
		return nil, err
	}

	imagePullPolicies := e.Build.Image.PullPolicies

	// Fetch image
	image, err := e.pullManager.GetDockerImage(imageName, e.Build.Image.ExecutorOptions.Docker, imagePullPolicies)
	if err != nil {
		return nil, err
	}

	return image, nil
}

func fakeContainer(id string, names ...string) *types.Container {
	return &types.Container{ID: id, Names: names}
}

func (e *executor) parseDeviceString(deviceString string) (device container.DeviceMapping, err error) {
	// Split the device string PathOnHost[:PathInContainer[:CgroupPermissions]]
	parts := strings.Split(deviceString, ":")

	if len(parts) > 3 {
		err = fmt.Errorf("too many colons")
		return
	}

	device.PathOnHost = parts[0]

	// Optional container path
	if len(parts) >= 2 {
		device.PathInContainer = parts[1]
	} else {
		// default: device at same path in container
		device.PathInContainer = device.PathOnHost
	}

	// Optional permissions
	if len(parts) >= 3 {
		device.CgroupPermissions = parts[2]
	} else {
		// default: rwm, just like 'docker run'
		device.CgroupPermissions = "rwm"
	}

	return
}

func (e *executor) bindDevices() (err error) {
	for _, deviceString := range e.Config.Docker.Devices {
		device, err := e.parseDeviceString(deviceString)
		if err != nil {
			err = fmt.Errorf("failed to parse device string %q: %w", deviceString, err)
			return err
		}

		e.devices = append(e.devices, device)
	}
	return nil
}

func (e *executor) bindDeviceRequests() error {
	if e.Config.Docker.Gpus == "" {
		return nil
	}

	var gpus opts.GpuOpts

	err := gpus.Set(e.Config.Docker.Gpus)
	if err != nil {
		return fmt.Errorf("parsing deviceRequest string %q: %w", e.Config.Docker.Gpus, err)
	}

	e.deviceRequests = gpus.Value()

	return nil
}

func isInAllowedPrivilegedImages(image string, allowedPrivilegedImages []string) bool {
	if len(allowedPrivilegedImages) == 0 {
		return true
	}
	for _, allowedImage := range allowedPrivilegedImages {
		ok, _ := doublestar.Match(allowedImage, image)
		if ok {
			return true
		}
	}
	return false
}

func (e *executor) isInPrivilegedServiceList(serviceDefinition common.Image) bool {
	return isInAllowedPrivilegedImages(serviceDefinition.Name, e.Config.Docker.AllowedPrivilegedServices)
}

//nolint:funlen
func (e *executor) createService(
	serviceIndex int,
	service, version, image string,
	definition common.Image,
	linkNames []string,
) (*types.Container, error) {
	if service == "" {
		return nil, fmt.Errorf("invalid service name: %s", definition.Name)
	}

	if e.volumesManager == nil {
		return nil, errVolumesManagerUndefined
	}

	e.Println("Starting service", service+":"+version, "...")
	serviceImage, err := e.pullManager.GetDockerImage(image, definition.ExecutorOptions.Docker, definition.PullPolicies)
	if err != nil {
		return nil, err
	}

	serviceSlug := strings.ReplaceAll(service, "/", "__")
	containerName := fmt.Sprintf("%s-%s-%d", e.getProjectUniqRandomizedName(), serviceSlug, serviceIndex)

	// this will fail potentially some builds if there's name collision
	_ = e.removeContainer(e.Context, containerName)

	labels := map[string]string{
		"type":            labelServiceType,
		"service":         service,
		"service.version": version,
	}

	config := &container.Config{
		Image:  serviceImage.ID,
		Labels: e.labeler.Labels(labels),
		Env:    e.getServiceVariables(definition),
	}

	if len(definition.Command) > 0 {
		config.Cmd = definition.Command
	}
	config.Entrypoint = e.overwriteEntrypoint(&definition)
	hostConfig := e.createHostConfigForService()
	hostConfig.Privileged = hostConfig.Privileged && e.isInPrivilegedServiceList(definition)

	if e.Build.IsFeatureFlagOn(featureflags.UseInitWithDockerExecutor) {
		hostConfig.Init = &useInit
	}

	networkConfig := e.networkConfig(linkNames)
	var platform *v1.Platform
	if definition.ExecutorOptions.Docker.Platform != "" {
		platform = platformForImage(*serviceImage)
	}

	e.Debugln("Creating service container", containerName, "...")
	resp, err := e.client.ContainerCreate(e.Context, config, hostConfig, networkConfig, platform, containerName)
	if err != nil {
		return nil, err
	}

	e.Debugln(fmt.Sprintf("Starting service container %s (%s)...", containerName, resp.ID))
	err = e.client.ContainerStart(e.Context, resp.ID, types.ContainerStartOptions{})
	if err != nil {
		e.temporary = append(e.temporary, resp.ID)
		return nil, err
	}

	return fakeContainer(resp.ID, containerName), nil
}

func platformForImage(image types.ImageInspect) *v1.Platform {
	return &v1.Platform{
		Architecture: image.Architecture,
		OS:           image.Os,
		OSVersion:    image.OsVersion,
		Variant:      image.Variant,
	}
}

func (e *executor) createHostConfigForService() *container.HostConfig {
	privileged := e.Config.Docker.Privileged
	if e.Config.Docker.ServicesPrivileged != nil {
		privileged = *e.Config.Docker.ServicesPrivileged
	}

	return &container.HostConfig{
		DNS:           e.Config.Docker.DNS,
		DNSSearch:     e.Config.Docker.DNSSearch,
		RestartPolicy: neverRestartPolicy,
		ExtraHosts:    e.Config.Docker.ExtraHosts,
		Privileged:    privileged,
		SecurityOpt:   e.Config.Docker.ServicesSecurityOpt,
		Runtime:       e.Config.Docker.Runtime,
		UsernsMode:    container.UsernsMode(e.Config.Docker.UsernsMode),
		NetworkMode:   e.networkMode,
		Binds:         e.volumesManager.Binds(),
		ShmSize:       e.Config.Docker.ShmSize,
		Tmpfs:         e.Config.Docker.ServicesTmpfs,
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}
}

func (e *executor) networkConfig(aliases []string) *network.NetworkingConfig {
	if e.networkMode.UserDefined() == "" {
		return &network.NetworkingConfig{}
	}

	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			e.networkMode.UserDefined(): {Aliases: aliases},
		},
	}
}

func (e *executor) getProjectUniqRandomizedName() string {
	if e.projectUniqRandomizedName == "" {
		uuid, _ := helpers.GenerateRandomUUID(8)
		e.projectUniqRandomizedName = fmt.Sprintf("%s-%s", e.Build.ProjectUniqueName(), uuid)
	}

	return e.projectUniqRandomizedName
}

func (e *executor) createBuildNetwork() error {
	if e.networksManager == nil {
		return errNetworksManagerUndefined
	}

	networkMode, err := e.networksManager.Create(e.Context, e.Config.Docker.NetworkMode, e.Config.Docker.EnableIPv6)
	if err != nil {
		return err
	}

	e.networkMode = networkMode

	return nil
}

func (e *executor) cleanupNetwork(ctx context.Context) error {
	if e.networksManager == nil {
		return errNetworksManagerUndefined
	}

	if e.networkMode.UserDefined() == "" {
		return nil
	}

	inspectResponse, err := e.networksManager.Inspect(ctx)
	if err != nil {
		e.Errorln("network inspect returned error ", err)
		return nil
	}

	for id := range inspectResponse.Containers {
		e.Debugln("Removing Container", id, "...")
		err = e.removeContainer(ctx, id)
		if err != nil {
			e.Errorln("remove container returned error ", err)
		}
	}

	return e.networksManager.Cleanup(ctx)
}

func (e *executor) isInPrivilegedImageList(imageDefinition common.Image) bool {
	return isInAllowedPrivilegedImages(imageDefinition.Name, e.Config.Docker.AllowedPrivilegedImages)
}

//nolint:funlen
func (e *executor) createContainer(
	containerType string,
	imageDefinition common.Image,
	cmd, allowedInternalImages []string,
) (*types.ContainerJSON, error) {
	if e.volumesManager == nil {
		return nil, errVolumesManagerUndefined
	}

	image, err := e.expandAndGetDockerImage(
		imageDefinition.Name,
		allowedInternalImages,
		imageDefinition.ExecutorOptions.Docker,
		imageDefinition.PullPolicies,
	)
	if err != nil {
		return nil, err
	}

	hostname := e.Config.Docker.Hostname
	if hostname == "" {
		hostname = e.Build.ProjectUniqueName()
	}

	// Build and predefined container names are comprised of:
	// - A runner project scoped ID (runner-<description>-project-<project_id>-concurrent-<concurrent>)
	// - A unique randomized ID for each execution
	// - The container's type (build, predefined)
	//
	// For example: runner-linux-project-123-concurrent-2-0a1b2c3d-predefined
	//
	// A container of the same type is created _once_ per execution and re-used.
	containerName := e.getProjectUniqRandomizedName() + "-" + containerType

	config := e.createContainerConfig(containerType, imageDefinition, image.ID, hostname, cmd)

	hostConfig, err := e.createHostConfig()
	if err != nil {
		return nil, err
	}
	hostConfig.Privileged = hostConfig.Privileged && e.isInPrivilegedImageList(imageDefinition)

	if containerType == buildContainerType && e.Build.IsFeatureFlagOn(featureflags.UseInitWithDockerExecutor) {
		hostConfig.Init = &useInit
	}

	aliases := []string{"build", containerName}
	networkConfig := e.networkConfig(aliases)

	var platform *v1.Platform
	if imageDefinition.ExecutorOptions.Docker.Platform != "" {
		platform = platformForImage(*image)
	}

	// this will fail potentially some builds if there's name collision
	_ = e.removeContainer(e.Context, containerName)

	e.Debugln("Creating container", containerName, "...")
	resp, err := e.client.ContainerCreate(e.Context, config, hostConfig, networkConfig, platform, containerName)
	if resp.ID != "" {
		e.temporary = append(e.temporary, resp.ID)
		if containerType == buildContainerType {
			e.buildContainerID = resp.ID
		}
	}
	if err != nil {
		return nil, err
	}

	inspect, err := e.client.ContainerInspect(e.Context, resp.ID)
	return &inspect, err
}

func (e *executor) createContainerConfig(
	containerType string,
	imageDefinition common.Image,
	imageID string,
	hostname string,
	cmd []string,
) *container.Config {
	labels := e.prepareContainerLabels(map[string]string{"type": containerType})
	config := &container.Config{
		Image:        imageID,
		Hostname:     hostname,
		Cmd:          cmd,
		Labels:       labels,
		Tty:          false,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		Env:          e.Build.GetAllVariables().StringList(),
	}

	// user config should only be set in build containers
	if containerType == buildContainerType {
		config.User = e.Config.Docker.User
	}

	config.Entrypoint = e.overwriteEntrypoint(&imageDefinition)
	config.MacAddress = e.Config.Docker.MacAddress

	return config
}

func (e *executor) createHostConfig() (*container.HostConfig, error) {
	nanoCPUs, err := e.Config.Docker.GetNanoCPUs()
	if err != nil {
		return nil, err
	}

	isolation := container.Isolation(e.Config.Docker.Isolation)
	if !isolation.IsValid() {
		return nil, fmt.Errorf("the isolation value %q is not valid. "+
			"the valid values are: 'process', 'hyperv', 'default' and an empty string", isolation)
	}

	ulimits, err := e.Config.Docker.GetUlimits()
	if err != nil {
		return nil, err
	}

	return &container.HostConfig{
		Resources: container.Resources{
			Memory:            e.Config.Docker.GetMemory(),
			MemorySwap:        e.Config.Docker.GetMemorySwap(),
			MemoryReservation: e.Config.Docker.GetMemoryReservation(),
			CpusetCpus:        e.Config.Docker.CPUSetCPUs,
			CPUShares:         e.Config.Docker.CPUShares,
			NanoCPUs:          nanoCPUs,
			Devices:           e.devices,
			DeviceRequests:    e.deviceRequests,
			OomKillDisable:    e.Config.Docker.GetOomKillDisable(),
			DeviceCgroupRules: e.Config.Docker.DeviceCgroupRules,
			Ulimits:           ulimits,
		},
		DNS:           e.Config.Docker.DNS,
		DNSSearch:     e.Config.Docker.DNSSearch,
		Runtime:       e.Config.Docker.Runtime,
		Privileged:    e.Config.Docker.Privileged,
		GroupAdd:      e.Config.Docker.GroupAdd,
		UsernsMode:    container.UsernsMode(e.Config.Docker.UsernsMode),
		CapAdd:        e.Config.Docker.CapAdd,
		CapDrop:       e.Config.Docker.CapDrop,
		SecurityOpt:   e.Config.Docker.SecurityOpt,
		RestartPolicy: neverRestartPolicy,
		ExtraHosts:    e.Config.Docker.ExtraHosts,
		NetworkMode:   e.networkMode,
		IpcMode:       container.IpcMode(e.Config.Docker.IpcMode),
		Links:         append(e.Config.Docker.Links, e.links...),
		Binds:         e.volumesManager.Binds(),
		OomScoreAdj:   e.Config.Docker.OomScoreAdjust,
		ShmSize:       e.Config.Docker.ShmSize,
		Isolation:     isolation,
		VolumeDriver:  e.Config.Docker.VolumeDriver,
		VolumesFrom:   e.Config.Docker.VolumesFrom,
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
		Tmpfs:   e.Config.Docker.Tmpfs,
		Sysctls: e.Config.Docker.SysCtls,
	}, nil
}

func (e *executor) startAndWatchContainer(ctx context.Context, id string, input io.Reader) error {
	dockerExec := exec.NewDocker(e.Context, e.client, e.waiter, e.Build.Log())

	streams := exec.IOStreams{
		Stdin:  input,
		Stderr: e.Trace,
		Stdout: e.Trace,
	}

	var gracefulExitFunc wait.GracefulExitFunc
	if id == e.buildContainerID {
		// send SIGTERM to all processes in the build container.
		gracefulExitFunc = e.sendSIGTERMToContainerProcs
	}
	return dockerExec.Exec(ctx, id, streams, gracefulExitFunc)
}

func (e *executor) removeContainer(ctx context.Context, id string) error {
	e.Debugln("Removing container", id)

	e.disconnectNetwork(ctx, id)

	options := types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}

	err := e.client.ContainerRemove(ctx, id, options)
	if err != nil {
		e.Debugln("Removing container", id, "finished with error", err)
		return err
	}

	e.Debugln("Removed container", id)
	return nil
}

func (e *executor) disconnectNetwork(ctx context.Context, id string) {
	e.Debugln("Disconnecting container", id, "from networks")

	netList, err := e.client.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		e.Debugln("Can't get network list. ListNetworks exited with", err)
		return
	}

	for _, network := range netList {
		for _, pluggedContainer := range network.Containers {
			if id == pluggedContainer.Name {
				err = e.client.NetworkDisconnect(ctx, network.ID, id, true)
				if err != nil {
					e.Warningln(
						"Can't disconnect possibly zombie container",
						pluggedContainer.Name,
						"from network",
						network.Name,
						"->",
						err,
					)
				} else {
					e.Warningln(
						"Possibly zombie container",
						pluggedContainer.Name,
						"is disconnected from network",
						network.Name,
					)
				}
				break
			}
		}
	}
}

func (e *executor) verifyAllowedImage(image, optionName string, allowedImages, internalImages []string) error {
	options := common.VerifyAllowedImageOptions{
		Image:          image,
		OptionName:     optionName,
		AllowedImages:  allowedImages,
		InternalImages: internalImages,
	}
	return common.VerifyAllowedImage(options, e.BuildLogger)
}

func (e *executor) expandImageName(imageName string, allowedInternalImages []string) (string, error) {
	defaultDockerImage := e.ExpandValue(e.Config.Docker.Image)
	if imageName != "" {
		image := e.ExpandValue(imageName)
		allowedInternalImages = append(allowedInternalImages, defaultDockerImage)
		err := e.verifyAllowedImage(image, "images", e.Config.Docker.AllowedImages, allowedInternalImages)
		if err != nil {
			return "", err
		}
		return image, nil
	}

	if defaultDockerImage == "" {
		return "", errors.New("no Docker image specified to run the build in")
	}

	return defaultDockerImage, nil
}

func (e *executor) overwriteEntrypoint(image *common.Image) []string {
	if len(image.Entrypoint) > 0 {
		if !e.Config.Docker.DisableEntrypointOverwrite {
			return image.Entrypoint
		}

		e.Warningln("Entrypoint override disabled")
	}

	return nil
}

//nolint:funlen,nestif
func (e *executor) connectDocker(options common.ExecutorPrepareOptions) error {
	var opts []client.Opt

	creds := e.Config.Docker.Credentials

	environment, ok := e.Build.ExecutorData.(executors.Environment)
	if ok {
		c, err := environment.Prepare(e.Context, e.BuildLogger, options)
		if err != nil {
			return fmt.Errorf("preparing environment: %w", err)
		}

		if e.tunnelClient != nil {
			e.tunnelClient.Close()
		}
		e.tunnelClient = c

		// We tunnel the docker connection for remote environments.
		//
		// To do this, we create a new dial context for Docker's client, whilst
		// also overridding the daemon hostname it would typically use (if it were to use
		// its own dialer).
		host := creds.Host
		scheme, dialer, err := e.environmentDialContext(c, host)
		if err != nil {
			return fmt.Errorf("creating env dialer: %w", err)
		}

		// If the scheme (docker uses it to define the protocol used) is "npipe" or "unix", we
		// need to use a "fake" host, otherwise when dialing from Linux to Windows or vice-versa
		// docker will complain because it doesn't think Linux can support "npipe" and doesn't
		// think Windows can support "unix".
		switch scheme {
		case "unix", "npipe":
			creds.Host = internalFakeTunnelHostname
		}

		opts = append(opts, client.WithDialContext(dialer))
	}

	dockerClient, err := docker.New(creds, opts...)
	if err != nil {
		return err
	}
	e.client = dockerClient

	e.info, err = e.client.Info(e.Context)
	if err != nil {
		return err
	}

	e.Debugln(fmt.Sprintf(
		"Connected to docker daemon (api version: %s, server version: %s, kernel: %s, os: %s/%s)",
		e.client.ClientVersion(),
		e.info.ServerVersion,
		e.info.KernelVersion,
		e.info.OSType,
		e.info.Architecture,
	))

	err = e.validateOSType()
	if err != nil {
		return err
	}

	e.waiter = wait.NewDockerKillWaiter(e.client)

	return err
}

func (e *executor) environmentDialContext(
	executorClient executors.Client,
	host string,
) (string, func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = client.DefaultDockerHost
	}
	u, err := client.ParseHostURL(host)
	if err != nil {
		return "", nil, fmt.Errorf("parsing docker host: %w", err)
	}

	return u.Scheme, func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := executorClient.Dial(u.Scheme, u.Host)
		if err != nil {
			return nil, fmt.Errorf("dialing environment connection: %w", err)
		}

		return conn, nil
	}, nil
}

// validateOSType checks if the ExecutorOptions metadata matches with the docker
// info response.
func (e *executor) validateOSType() error {
	switch e.info.OSType {
	case osTypeLinux, osTypeWindows:
		return nil
	}

	return fmt.Errorf("unsupported os type: %s", e.info.OSType)
}

func (e *executor) createDependencies() error {
	createDependenciesStrategy := []func() error{
		e.createLabeler,
		e.createNetworksManager,
		e.createBuildNetwork,
		e.createPullManager,
		e.bindDevices,
		e.bindDeviceRequests,
		e.createVolumesManager,
		e.createVolumes,
		e.createBuildVolume,
		e.createServices,
	}

	for _, setup := range createDependenciesStrategy {
		err := setup()
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *executor) createVolumes() error {
	e.SetCurrentStage(ExecutorStageCreatingUserVolumes)
	e.Debugln("Creating user-defined volumes...")

	if e.volumesManager == nil {
		return errVolumesManagerUndefined
	}

	for _, volume := range e.Config.Docker.Volumes {
		err := e.volumesManager.Create(e.Context, volume)
		if errors.Is(err, volumes.ErrCacheVolumesDisabled) {
			e.Warningln(fmt.Sprintf(
				"Container based cache volumes creation is disabled. Will not create volume for %q",
				volume,
			))
			continue
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (e *executor) createBuildVolume() error {
	e.SetCurrentStage(ExecutorStageCreatingBuildVolumes)
	e.Debugln("Creating build volume...")

	if e.volumesManager == nil {
		return errVolumesManagerUndefined
	}

	jobsDir := e.Build.RootDir

	var err error

	if e.Build.GetGitStrategy() == common.GitFetch {
		err = e.volumesManager.Create(e.Context, jobsDir)
		if err == nil {
			return nil
		}

		if errors.Is(err, volumes.ErrCacheVolumesDisabled) {
			err = e.volumesManager.CreateTemporary(e.Context, jobsDir)
		}
	} else {
		err = e.volumesManager.CreateTemporary(e.Context, jobsDir)
	}

	if err != nil {
		var volDefinedErr *volumes.ErrVolumeAlreadyDefined
		if !errors.As(err, &volDefinedErr) {
			return err
		}
	}

	return nil
}

func (e *executor) Prepare(options common.ExecutorPrepareOptions) error {
	e.SetCurrentStage(ExecutorStagePrepare)

	if options.Config.Docker == nil {
		return errors.New("missing docker configuration")
	}

	e.AbstractExecutor.PrepareConfiguration(options)

	err := e.connectDocker(options)
	if err != nil {
		return err
	}

	e.helperImageInfo, err = e.prepareHelperImage()
	if err != nil {
		return err
	}

	// setup default executor options based on OS type
	e.setupDefaultExecutorOptions(e.helperImageInfo.OSType)

	err = e.prepareBuildsDir(options)
	if err != nil {
		return err
	}

	err = e.AbstractExecutor.PrepareBuildAndShell()
	if err != nil {
		return err
	}

	if e.BuildShell.PassFile {
		return errors.New("docker doesn't support shells that require script file")
	}

	imageName, err := e.expandImageName(e.Build.Image.Name, []string{})
	if err != nil {
		return err
	}

	e.Println("Using Docker executor with image", imageName, "...")

	err = e.createDependencies()
	if err != nil {
		return err
	}
	return nil
}

func (e *executor) setupDefaultExecutorOptions(os string) {
	switch os {
	case helperimage.OSTypeWindows:
		e.DefaultBuildsDir = `C:\builds`
		e.DefaultCacheDir = `C:\cache`

		e.ExecutorOptions.Shell.Shell = shells.SNPowershell
		e.ExecutorOptions.Shell.RunnerCommand = "gitlab-runner-helper"

		if e.volumeParser == nil {
			e.volumeParser = parser.NewWindowsParser()
		}

		if e.newVolumePermissionSetter == nil {
			e.newVolumePermissionSetter = func() (permission.Setter, error) {
				return permission.NewDockerWindowsSetter(), nil
			}
		}

	default:
		e.DefaultBuildsDir = `/builds`
		e.DefaultCacheDir = `/cache`

		e.ExecutorOptions.Shell.Shell = "bash"
		e.ExecutorOptions.Shell.RunnerCommand = "/usr/bin/gitlab-runner-helper"

		if e.volumeParser == nil {
			e.volumeParser = parser.NewLinuxParser()
		}

		if e.newVolumePermissionSetter == nil {
			e.newVolumePermissionSetter = func() (permission.Setter, error) {
				helperImage, err := e.getPrebuiltImage()
				if err != nil {
					return nil, err
				}

				return permission.NewDockerLinuxSetter(e.client, e.Build.Log(), helperImage), nil
			}
		}
	}
}

func (e *executor) prepareHelperImage() (helperimage.Info, error) {
	return helperimage.Get(common.REVISION, helperimage.Config{
		OSType:        e.info.OSType,
		Architecture:  e.info.Architecture,
		KernelVersion: e.info.KernelVersion,
		Shell:         e.Config.Shell,
		Flavor:        e.ExpandValue(e.Config.Docker.HelperImageFlavor),
	})
}

func (e *executor) prepareBuildsDir(options common.ExecutorPrepareOptions) error {
	if e.volumeParser == nil {
		return common.MakeBuildError("missing volume parser")
	}

	isHostMounted, err := volumes.IsHostMountedVolume(e.volumeParser, e.RootDir(), options.Config.Docker.Volumes...)
	if err != nil {
		return &common.BuildError{Inner: err}
	}

	// We need to set proper value for e.SharedBuildsDir because
	// it's required to properly start the job, what is done inside of
	// e.AbstractExecutor.Prepare()
	// And a started job is required for Volumes Manager to work, so it's
	// done before the manager is even created.
	if isHostMounted {
		e.SharedBuildsDir = true
	}

	return nil
}

//nolint:funlen
func (e *executor) Cleanup() {
	if e.Config.Docker == nil {
		// if there's no Docker config, we got here because Prepare() failed
		// and there's nothing to cleanup.
		return
	}

	e.SetCurrentStage(ExecutorStageCleanup)

	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()

	remove := func(id string) {
		wg.Add(1)
		go func() {
			_ = e.removeContainer(ctx, id)
			wg.Done()
		}()
	}

	for _, temporaryID := range e.temporary {
		remove(temporaryID)
	}

	wg.Wait()

	err := e.cleanupVolume(ctx)
	if err != nil {
		volumeLogger := e.WithFields(logrus.Fields{
			"error": err,
		})

		volumeLogger.Errorln("Failed to cleanup volumes")
	}

	err = e.cleanupNetwork(ctx)
	if err != nil {
		networkLogger := e.WithFields(logrus.Fields{
			"network": e.networkMode.NetworkName(),
			"error":   err,
		})

		networkLogger.Errorln("Failed to remove network for build")
	}

	if e.client != nil {
		err = e.client.Close()
		if err != nil {
			clientCloseLogger := e.WithFields(logrus.Fields{
				"error": err,
			})

			clientCloseLogger.Debugln("Failed to close the client")
		}
	}

	if e.tunnelClient != nil {
		e.tunnelClient.Close()
	}

	e.AbstractExecutor.Cleanup()
}

// sendSIGTERMToContainerProcs exec's into the specified container and executes the script
// shells.sendSIGTERMToContainerProcs, which (unsurprisingly) sends SIGTERM to all processes in the container. This
// Effectively gives the processes in a the container a chance to exit gracefully (if they listen for SIGTERM).
func (e *executor) sendSIGTERMToContainerProcs(ctx context.Context, containerID string) error {
	e.Debugln("Emitting SIGTERM to processes in container", containerID)
	return e.execScriptOnContainer(ctx, containerID, shells.ContainerSigTermScript)
}

func (e *executor) execScriptOnContainer(ctx context.Context, containerID string, script ...string) error {
	execConfig := types.ExecConfig{
		Tty:          true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          append([]string{"sh", "-c"}, script...),
	}

	exec, err := e.client.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		l := e.Config.Log().WithFields(logrus.Fields{"error": err})
		l.Warningln("Failed to exec create to container:", err)
		return err
	}

	resp, err := e.client.ContainerExecAttach(ctx, exec.ID, types.ExecStartCheck{})
	if err != nil {
		l := e.Config.Log().WithFields(logrus.Fields{"error": err})
		l.Warningln("Failed to exec attach to container:", err)
		return err
	}
	defer resp.Close()

	// Copy any output generated by running the script (typically there will be none) to runner's stdout/stderr...
	_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, resp.Reader)
	if err != nil {
		l := e.Config.Log().WithFields(logrus.Fields{"error": err})
		l.Warningln("Failed to read from attached container:", err)
		return err
	}

	return nil
}

func (e *executor) cleanupVolume(ctx context.Context) error {
	if e.volumesManager == nil {
		e.Debugln("Volumes manager is empty, skipping volumes cleanup")
		return nil
	}

	err := e.volumesManager.RemoveTemporary(ctx)
	if err != nil {
		return fmt.Errorf("remove temporary volumes: %w", err)
	}

	return nil
}

func (e *executor) createHostConfigForServiceHealthCheck(service *types.Container) *container.HostConfig {
	var legacyLinks []string
	if e.networkMode.UserDefined() == "" {
		legacyLinks = append(legacyLinks, service.Names[0]+":service")
	}

	return &container.HostConfig{
		RestartPolicy: neverRestartPolicy,
		Links:         legacyLinks,
		NetworkMode:   e.networkMode,
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}
}

// addServiceHealthCheckEnvironment returns environment variables mimicing
// the legacy container links networking feature of Docker, where environment
// variables are provided with the hostname and port of the linked service our
// health check is performed against.
//
// The hostname we provide is the container's short ID (the first 12 characters
// of a full container ID). The short ID, as opposed to the full ID, is
// internally resolved to the container's IP address by Docker's built-in DNS
// service.
//
// The legacy container links (https://docs.docker.com/network/links/) network
// feature is deprecated. When we remove support for links, the healthcheck
// system can be updated to no longer rely on environment variables
func (e *executor) addServiceHealthCheckEnvironment(service *types.Container) ([]string, error) {
	environment := []string{}

	if e.networkMode.UserDefined() != "" {
		environment = append(environment, "WAIT_FOR_SERVICE_TCP_ADDR="+service.ID[:12])
		ports, err := e.getContainerExposedPorts(service)
		if err != nil {
			return nil, fmt.Errorf("get container exposed ports: %v", err)
		}
		if len(ports) == 0 {
			return nil, fmt.Errorf("service %q has no exposed ports", service.Names[0])
		}

		for _, port := range ports {
			environment = append(environment, fmt.Sprintf("WAIT_FOR_SERVICE_%d_TCP_PORT=%d", port, port))
		}
	}

	return environment, nil
}

//nolint:gocognit
func (e *executor) getContainerExposedPorts(container *types.Container) ([]int, error) {
	inspect, err := e.client.ContainerInspect(e.Context, container.ID)
	if err != nil {
		return nil, err
	}

	for _, env := range inspect.Config.Env {
		key, val, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}

		if strings.EqualFold(key, "HEALTHCHECK_TCP_PORT") {
			port, err := strconv.ParseInt(val, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid health check tcp port: %v", val)
			}

			return []int{int(port)}, nil
		}
	}

	// maxPortsCheck is the maximum number of ports that we'll check to see
	// if a service is running
	const maxPortsCheck = 20

	var ports []int
	for port := range inspect.Config.ExposedPorts {
		start, end, err := port.Range()
		if err == nil && port.Proto() == "tcp" {
			for i := start; i <= end && len(ports) < maxPortsCheck; i++ {
				ports = append(ports, i)
			}
		}
	}

	sort.Ints(ports)

	return ports, nil
}

func (e *executor) readContainerLogs(containerID string) string {
	var buf bytes.Buffer

	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
	}

	hijacked, err := e.client.ContainerLogs(e.Context, containerID, options)
	if err != nil {
		return strings.TrimSpace(err.Error())
	}
	defer func() { _ = hijacked.Close() }()

	// limit how much data we read from the container log to
	// avoid memory exhaustion
	w := limitwriter.New(&buf, ServiceLogOutputLimit)

	_, _ = stdcopy.StdCopy(w, w, hijacked)
	return strings.TrimSpace(buf.String())
}

func (e *executor) prepareContainerLabels(otherLabels map[string]string) map[string]string {
	l := e.labeler.Labels(otherLabels)

	for k, v := range e.Config.Docker.ContainerLabels {
		l[k] = e.Build.Variables.ExpandValue(v)
	}

	return l
}
