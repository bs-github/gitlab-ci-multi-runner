package docker

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/executors"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
	docker_helpers "gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers/docker"

	"golang.org/x/net/context"
)

var neverRestartPolicy = container.RestartPolicy{Name: "no"}

type dockerOptions struct {
	Image    string   `json:"image"`
	Services []string `json:"services"`
}

type executor struct {
	executors.AbstractExecutor
	client      docker_helpers.Client
	failures    []string // IDs of containers that have failed in some way
	builds      []*types.Container
	services    []*types.Container
	caches      []string // IDs of cache containers
	options     dockerOptions
	info        types.Info
	binds       []string
	volumesFrom []string
	devices     []container.DeviceMapping
	links       []string
}

func (s *executor) getServiceVariables() []string {
	return s.Build.GetAllVariables().PublicOrInternal().StringList()
}

func (s *executor) getUserAuthConfiguration(indexName string) *types.AuthConfig {
	if s.Build == nil {
		return nil
	}

	buf := bytes.NewBufferString(s.Build.GetDockerAuthConfig())
	authConfigs, _ := docker_helpers.ReadAuthConfigsFromReader(buf)
	if authConfigs != nil {
		return docker_helpers.ResolveDockerAuthConfig(indexName, authConfigs)
	}
	return nil
}

func (s *executor) getBuildAuthConfiguration(indexName string) *types.AuthConfig {
	if s.Build == nil {
		return nil
	}

	authConfigs := make(map[string]types.AuthConfig)

	for _, credentials := range s.Build.Credentials {
		if credentials.Type != "registry" {
			continue
		}

		authConfigs[credentials.URL] = types.AuthConfig{
			Username:      credentials.Username,
			Password:      credentials.Password,
			ServerAddress: credentials.URL,
		}
	}

	if authConfigs != nil {
		return docker_helpers.ResolveDockerAuthConfig(indexName, authConfigs)
	}
	return nil
}

func (s *executor) getHomeDirAuthConfiguration(indexName string) *types.AuthConfig {
	authConfigs, _ := docker_helpers.ReadDockerAuthConfigsFromHomeDir(s.Shell().User)
	if authConfigs != nil {
		return docker_helpers.ResolveDockerAuthConfig(indexName, authConfigs)
	}
	return nil
}

func (s *executor) getAuthConfig(imageName string) *types.AuthConfig {
	indexName, _ := docker_helpers.SplitDockerImageName(imageName)

	authConfig := s.getUserAuthConfiguration(indexName)
	if authConfig == nil {
		authConfig = s.getHomeDirAuthConfiguration(indexName)
	}
	if authConfig == nil {
		authConfig = s.getBuildAuthConfiguration(indexName)
	}

	if authConfig != nil {
		s.Debugln("Using", authConfig.Username, "to connect to", authConfig.ServerAddress,
			"in order to resolve", imageName, "...")
		return authConfig
	}

	s.Debugln(fmt.Sprintf("No credentials found for %v", indexName))
	return nil
}

func (s *executor) pullDockerImage(imageName string, ac *types.AuthConfig) (*types.ImageInspect, error) {
	s.Println("Pulling docker image", imageName, "...")

	ref := imageName
	// Add :latest to limit the download results
	if !strings.ContainsAny(ref, ":@") {
		ref += ":latest"
	}

	options := types.ImagePullOptions{}
	if ac != nil {
		options.RegistryAuth, _ = docker_helpers.EncodeAuthConfig(ac)
	}

	if err := s.client.ImagePullBlocking(context.TODO(), ref, options); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, &common.BuildError{Inner: err}
		}
		return nil, err
	}

	image, _, err := s.client.ImageInspectWithRaw(context.TODO(), imageName)
	return &image, err
}

func (s *executor) getDockerImage(imageName string) (*types.ImageInspect, error) {
	pullPolicy, err := s.Config.Docker.PullPolicy.Get()
	if err != nil {
		return nil, err
	}

	authConfig := s.getAuthConfig(imageName)

	s.Debugln("Looking for image", imageName, "...")
	image, _, err := s.client.ImageInspectWithRaw(context.TODO(), imageName)

	// If never is specified then we return what inspect did return
	if pullPolicy == common.PullPolicyNever {
		return &image, err
	}

	if err == nil {
		// Don't pull image that is passed by ID
		if image.ID == imageName {
			return &image, nil
		}

		// If not-present is specified
		if pullPolicy == common.PullPolicyIfNotPresent {
			s.Println("Using locally found image version due to if-not-present pull policy")
			return &image, err
		}
	}

	newImage, err := s.pullDockerImage(imageName, authConfig)
	if err != nil {
		return nil, err
	}
	return newImage, nil
}

func (s *executor) getArchitecture() string {
	architecture := s.info.Architecture
	switch architecture {
	case "armv6l", "armv7l", "aarch64":
		architecture = "arm"
	case "amd64":
		architecture = "x86_64"
	}

	if architecture != "" {
		return architecture
	}

	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

func (s *executor) getPrebuiltImage() (*types.ImageInspect, error) {
	architecture := s.getArchitecture()
	if architecture == "" {
		return nil, errors.New("unsupported docker architecture")
	}

	imageName := prebuiltImageName + ":" + architecture + "-" + common.REVISION
	s.Debugln("Looking for prebuilt image", imageName, "...")
	image, _, err := s.client.ImageInspectWithRaw(context.TODO(), imageName)
	if err == nil {
		return &image, nil
	}

	data, err := Asset("prebuilt-" + architecture + prebuiltImageExtension)
	if err != nil {
		return nil, fmt.Errorf("Unsupported architecture: %s: %q", architecture, err.Error())
	}

	s.Debugln("Loading prebuilt image...")

	ref := prebuiltImageName
	source := types.ImageImportSource{
		Source:     bytes.NewBuffer(data),
		SourceName: "-",
	}
	options := types.ImageImportOptions{
		Tag: architecture + "-" + common.REVISION,
	}

	if err := s.client.ImageImportBlocking(context.TODO(), source, ref, options); err != nil {
		return nil, fmt.Errorf("Failed to import image: %s", err)
	}

	image, _, err = s.client.ImageInspectWithRaw(context.TODO(), imageName)
	if err != nil {
		s.Debugln("Inspecting imported image", imageName, "failed:", err)
		return nil, err
	}

	return &image, err
}

func (s *executor) getAbsoluteContainerPath(dir string) string {
	if path.IsAbs(dir) {
		return dir
	}
	return path.Join(s.Build.FullProjectDir(), dir)
}

func (s *executor) addHostVolume(hostPath, containerPath string) error {
	containerPath = s.getAbsoluteContainerPath(containerPath)
	s.Debugln("Using host-based", hostPath, "for", containerPath, "...")
	s.binds = append(s.binds, fmt.Sprintf("%v:%v", hostPath, containerPath))
	return nil
}

func (s *executor) getLabels(containerType string, otherLabels ...string) map[string]string {
	labels := make(map[string]string)
	labels[dockerLabelPrefix+".build.id"] = strconv.Itoa(s.Build.ID)
	labels[dockerLabelPrefix+".build.sha"] = s.Build.Sha
	labels[dockerLabelPrefix+".build.before_sha"] = s.Build.BeforeSha
	labels[dockerLabelPrefix+".build.ref_name"] = s.Build.RefName
	labels[dockerLabelPrefix+".project.id"] = strconv.Itoa(s.Build.ProjectID)
	labels[dockerLabelPrefix+".runner.id"] = s.Build.Runner.ShortDescription()
	labels[dockerLabelPrefix+".runner.local_id"] = strconv.Itoa(s.Build.RunnerID)
	labels[dockerLabelPrefix+".type"] = containerType
	for _, label := range otherLabels {
		keyValue := strings.SplitN(label, "=", 2)
		if len(keyValue) == 2 {
			labels[dockerLabelPrefix+"."+keyValue[0]] = keyValue[1]
		}
	}
	return labels
}

// createCacheVolume returns the id of the created container, or an error
func (s *executor) createCacheVolume(containerName, containerPath string) (string, error) {
	// get busybox image
	cacheImage, err := s.getPrebuiltImage()
	if err != nil {
		return "", err
	}

	config := &container.Config{
		Image: cacheImage.ID,
		Cmd: []string{
			"gitlab-runner-cache", containerPath,
		},
		Volumes: map[string]struct{}{
			containerPath: {},
		},
		Labels: s.getLabels("cache", "cache.dir="+containerPath),
	}

	hostConfig := &container.HostConfig{
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}

	resp, err := s.client.ContainerCreate(context.TODO(), config, hostConfig, nil, containerName)
	if err != nil {
		if resp.ID != "" {
			s.failures = append(s.failures, resp.ID)
		}
		return "", err
	}

	s.Debugln("Starting cache container", resp.ID, "...")
	err = s.client.ContainerStart(context.TODO(), resp.ID, types.ContainerStartOptions{})
	if err != nil {
		s.failures = append(s.failures, resp.ID)
		return "", err
	}

	s.Debugln("Waiting for cache container", resp.ID, "...")
	err = s.waitForContainer(resp.ID)
	if err != nil {
		s.failures = append(s.failures, resp.ID)
		return "", err
	}

	return resp.ID, nil
}

func (s *executor) addCacheVolume(containerPath string) error {
	var err error
	containerPath = s.getAbsoluteContainerPath(containerPath)

	// disable cache for automatic container cache, but leave it for host volumes (they are shared on purpose)
	if s.Config.Docker.DisableCache {
		s.Debugln("Container cache for", containerPath, " is disabled.")
		return nil
	}

	hash := md5.Sum([]byte(containerPath))

	// use host-based cache
	if cacheDir := s.Config.Docker.CacheDir; cacheDir != "" {
		hostPath := fmt.Sprintf("%s/%s/%x", cacheDir, s.Build.ProjectUniqueName(), hash)
		hostPath, err := filepath.Abs(hostPath)
		if err != nil {
			return err
		}
		s.Debugln("Using path", hostPath, "as cache for", containerPath, "...")
		s.binds = append(s.binds, fmt.Sprintf("%v:%v", filepath.ToSlash(hostPath), containerPath))
		return nil
	}

	// get existing cache container
	var containerID string
	containerName := fmt.Sprintf("%s-cache-%x", s.Build.ProjectUniqueName(), hash)
	if inspected, err := s.client.ContainerInspect(context.TODO(), containerName); err == nil {
		// check if we have valid cache, if not remove the broken container
		if _, stale := inspected.Config.Volumes[containerPath]; stale {
			s.removeContainer(inspected.ID)
		} else {
			containerID = inspected.ID
		}
	}

	// create new cache container for that project
	if containerID == "" {
		containerID, err = s.createCacheVolume(containerName, containerPath)
		if err != nil {
			return err
		}
	}

	s.Debugln("Using container", containerID, "as cache", containerPath, "...")
	s.volumesFrom = append(s.volumesFrom, containerID)
	return nil
}

func (s *executor) addVolume(volume string) error {
	var err error
	hostVolume := strings.SplitN(volume, ":", 2)
	switch len(hostVolume) {
	case 2:
		err = s.addHostVolume(hostVolume[0], hostVolume[1])

	case 1:
		// disable cache disables
		err = s.addCacheVolume(hostVolume[0])
	}

	if err != nil {
		s.Errorln("Failed to create container volume for", volume, err)
	}
	return err
}

func fakeContainer(id string, names ...string) *types.Container {
	return &types.Container{ID: id, Names: names}
}

func (s *executor) createBuildVolume() error {
	// Cache Git sources:
	// take path of the projects directory,
	// because we use `rm -rf` which could remove the mounted volume
	parentDir := path.Dir(s.Build.FullProjectDir())

	if !path.IsAbs(parentDir) && parentDir != "/" {
		return errors.New("build directory needs to be absolute and non-root path")
	}

	if s.isHostMountedVolume(s.Build.RootDir, s.Config.Docker.Volumes...) {
		return nil
	}

	if s.Build.GetGitStrategy() == common.GitFetch && !s.Config.Docker.DisableCache {
		// create persistent cache container
		return s.addVolume(parentDir)
	}

	// create temporary cache container
	id, err := s.createCacheVolume("", parentDir)
	if err != nil {
		return err
	}

	s.caches = append(s.caches, id)
	s.volumesFrom = append(s.volumesFrom, id)

	return nil
}

func (s *executor) createUserVolumes() (err error) {
	for _, volume := range s.Config.Docker.Volumes {
		err = s.addVolume(volume)
		if err != nil {
			return
		}
	}
	return nil
}

func (s *executor) isHostMountedVolume(dir string, volumes ...string) bool {
	isParentOf := func(parent string, dir string) bool {
		for dir != "/" && dir != "." {
			if dir == parent {
				return true
			}
			dir = path.Dir(dir)
		}
		return false
	}

	for _, volume := range volumes {
		hostVolume := strings.Split(volume, ":")
		if len(hostVolume) < 2 {
			continue
		}

		if isParentOf(path.Clean(hostVolume[1]), path.Clean(dir)) {
			return true
		}
	}
	return false
}

func (s *executor) parseDeviceString(deviceString string) (device container.DeviceMapping, err error) {
	// Split the device string PathOnHost[:PathInContainer[:CgroupPermissions]]
	parts := strings.Split(deviceString, ":")

	if len(parts) > 3 {
		err = fmt.Errorf("Too many colons")
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

func (s *executor) bindDevices() (err error) {
	for _, deviceString := range s.Config.Docker.Devices {
		device, err := s.parseDeviceString(deviceString)
		if err != nil {
			err = fmt.Errorf("Failed to parse device string %q: %s", deviceString, err)
			return err
		}

		s.devices = append(s.devices, device)
	}
	return nil
}

func (s *executor) splitServiceAndVersion(serviceDescription string) (service, version, imageName string, linkNames []string) {
	ReferenceRegexpNoPort := regexp.MustCompile(`^(.*?)(|:[0-9]+)(|/.*)$`)
	imageName = serviceDescription
	version = "latest"

	if match := reference.ReferenceRegexp.FindStringSubmatch(serviceDescription); match != nil {
		matchService := ReferenceRegexpNoPort.FindStringSubmatch(match[1])
		service = matchService[1] + matchService[3]

		if len(match[2]) > 0 {
			version = match[2]
		} else {
			imageName = match[1] + ":" + version
		}
	} else {
		return
	}

	linkName := strings.Replace(service, "/", "__", -1)
	linkNames = append(linkNames, linkName)

	// Create alternative link name according to RFC 1123
	// Where you can use only `a-zA-Z0-9-`
	if alternativeName := strings.Replace(service, "/", "-", -1); linkName != alternativeName {
		linkNames = append(linkNames, alternativeName)
	}
	return
}

func (s *executor) createService(service, version, image string) (*types.Container, error) {
	if len(service) == 0 {
		return nil, errors.New("invalid service name")
	}

	s.Println("Starting service", service+":"+version, "...")
	serviceImage, err := s.getDockerImage(image)
	if err != nil {
		return nil, err
	}

	containerName := s.Build.ProjectUniqueName() + "-" + strings.Replace(service, "/", "__", -1)

	// this will fail potentially some builds if there's name collision
	s.removeContainer(containerName)

	config := &container.Config{
		Image:  serviceImage.ID,
		Labels: s.getLabels("service", "service="+service, "service.version="+version),
		Env:    s.getServiceVariables(),
	}

	hostConfig := &container.HostConfig{
		RestartPolicy: neverRestartPolicy,
		Privileged:    s.Config.Docker.Privileged,
		NetworkMode:   container.NetworkMode(s.Config.Docker.NetworkMode),
		Binds:         s.binds,
		VolumesFrom:   s.volumesFrom,
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}

	s.Debugln("Creating service container", containerName, "...")
	resp, err := s.client.ContainerCreate(context.TODO(), config, hostConfig, nil, containerName)
	if err != nil {
		return nil, err
	}

	s.Debugln("Starting service container", resp.ID, "...")
	err = s.client.ContainerStart(context.TODO(), resp.ID, types.ContainerStartOptions{})
	if err != nil {
		s.failures = append(s.failures, resp.ID)
		return nil, err
	}

	return fakeContainer(resp.ID, containerName), nil
}

func (s *executor) getServiceNames() ([]string, error) {
	services := s.Config.Docker.Services

	for _, service := range s.options.Services {
		service = s.Build.GetAllVariables().ExpandValue(service)
		err := s.verifyAllowedImage(service, "services", s.Config.Docker.AllowedServices, s.Config.Docker.Services)
		if err != nil {
			return nil, err
		}

		services = append(services, service)
	}

	return services, nil
}

func (s *executor) waitForServices() {
	waitForServicesTimeout := s.Config.Docker.WaitForServicesTimeout
	if waitForServicesTimeout == 0 {
		waitForServicesTimeout = common.DefaultWaitForServicesTimeout
	}

	// wait for all services to came up
	if waitForServicesTimeout > 0 && len(s.services) > 0 {
		s.Println("Waiting for services to be up and running...")
		wg := sync.WaitGroup{}
		for _, service := range s.services {
			wg.Add(1)
			go func(service *types.Container) {
				s.waitForServiceContainer(service, time.Duration(waitForServicesTimeout)*time.Second)
				wg.Done()
			}(service)
		}
		wg.Wait()
	}
}

func (s *executor) buildServiceLinks(linksMap map[string]*types.Container) (links []string) {
	for linkName, linkee := range linksMap {
		newContainer, err := s.client.ContainerInspect(context.TODO(), linkee.ID)
		if err != nil {
			continue
		}
		if newContainer.State.Running {
			links = append(links, linkee.ID+":"+linkName)
		}
	}
	return
}

func (s *executor) createFromServiceDescription(description string, linksMap map[string]*types.Container) (err error) {
	var container *types.Container

	service, version, imageName, linkNames := s.splitServiceAndVersion(description)

	for _, linkName := range linkNames {
		if linksMap[linkName] != nil {
			s.Warningln("Service", description, "is already created. Ignoring.")
			continue
		}

		// Create service if not yet created
		if container == nil {
			container, err = s.createService(service, version, imageName)
			if err != nil {
				return
			}
			s.Debugln("Created service", description, "as", container.ID)
			s.services = append(s.services, container)
		}
		linksMap[linkName] = container
	}
	return
}

func (s *executor) createServices() (err error) {
	serviceNames, err := s.getServiceNames()
	if err != nil {
		return
	}

	linksMap := make(map[string]*types.Container)

	for _, serviceDescription := range serviceNames {
		err = s.createFromServiceDescription(serviceDescription, linksMap)
		if err != nil {
			return
		}
	}

	s.waitForServices()

	s.links = s.buildServiceLinks(linksMap)
	return
}

func (s *executor) createContainer(containerType, imageName string, cmd []string) (*types.ContainerJSON, error) {
	// Fetch image
	image, err := s.getDockerImage(imageName)
	if err != nil {
		return nil, err
	}

	hostname := s.Config.Docker.Hostname
	if hostname == "" {
		hostname = s.Build.ProjectUniqueName()
	}

	containerName := s.Build.ProjectUniqueName() + "-" + containerType
	config := &container.Config{
		Image:        image.ID,
		Hostname:     hostname,
		Cmd:          cmd,
		Labels:       s.getLabels(containerType),
		Tty:          false,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		Env:          append(s.Build.GetAllVariables().StringList(), s.BuildShell.Environment...),
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			CpusetCpus: s.Config.Docker.CPUSetCPUs,
			Devices:    s.devices,
		},
		DNS:           s.Config.Docker.DNS,
		DNSSearch:     s.Config.Docker.DNSSearch,
		Privileged:    s.Config.Docker.Privileged,
		CapAdd:        s.Config.Docker.CapAdd,
		CapDrop:       s.Config.Docker.CapDrop,
		SecurityOpt:   s.Config.Docker.SecurityOpt,
		RestartPolicy: neverRestartPolicy,
		ExtraHosts:    s.Config.Docker.ExtraHosts,
		NetworkMode:   container.NetworkMode(s.Config.Docker.NetworkMode),
		Links:         append(s.Config.Docker.Links, s.links...),
		Binds:         s.binds,
		VolumeDriver:  s.Config.Docker.VolumeDriver,
		VolumesFrom:   append(s.Config.Docker.VolumesFrom, s.volumesFrom...),
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}

	// this will fail potentially some builds if there's name collision
	s.removeContainer(containerName)

	s.Debugln("Creating container", containerName, "...")
	resp, err := s.client.ContainerCreate(context.TODO(), config, hostConfig, nil, containerName)
	if err != nil {
		if resp.ID != "" {
			s.failures = append(s.failures, resp.ID)
		}
		return nil, err
	}

	inspect, err := s.client.ContainerInspect(context.TODO(), resp.ID)
	if err != nil {
		s.failures = append(s.failures, resp.ID)
		return nil, err
	}
	return &inspect, nil
}

func (s *executor) killContainer(id string, waitCh chan error) (err error) {
	for {
		s.disconnectNetwork(id)
		s.Debugln("Killing container", id, "...")
		s.client.ContainerKill(context.TODO(), id, "SIGKILL")

		// Wait for signal that container were killed
		// or retry after some time
		select {
		case err = <-waitCh:
			return

		case <-time.After(time.Second):
		}
	}
}

func (s *executor) waitForContainer(id string) error {
	s.Debugln("Waiting for container", id, "...")

	retries := 0

	// Use active wait
	for {
		container, err := s.client.ContainerInspect(context.TODO(), id)
		if err != nil {
			if docker_helpers.IsErrNotFound(err) {
				return err
			}

			if retries > 3 {
				return err
			}

			retries++
			time.Sleep(time.Second)
			continue
		}

		// Reset retry timer
		retries = 0

		if container.State.Running {
			time.Sleep(time.Second)
			continue
		}

		if container.State.ExitCode != 0 {
			return &common.BuildError{
				Inner: fmt.Errorf("exit code %d", container.State.ExitCode),
			}
		}

		return nil
	}
}

func (s *executor) watchContainer(id string, input io.Reader, abort chan interface{}) (err error) {
	options := types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	}

	s.Debugln("Attaching to container", id, "...")
	hijacked, err := s.client.ContainerAttach(context.TODO(), id, options)
	if err != nil {
		return
	}
	defer hijacked.Close()

	s.Debugln("Starting container", id, "...")
	err = s.client.ContainerStart(context.TODO(), id, types.ContainerStartOptions{})
	if err != nil {
		return
	}

	s.Debugln("Waiting for attach to finish", id, "...")
	attachCh := make(chan error, 2)

	// Copy any output to the build trace
	go func() {
		_, err := stdcopy.StdCopy(s.BuildTrace, s.BuildTrace, hijacked.Reader)
		if err != nil {
			attachCh <- err
		}
	}()

	// Write the input to the container and close its STDIN to get it to finish
	go func() {
		_, err := io.Copy(hijacked.Conn, input)
		hijacked.CloseWrite()
		if err != nil {
			attachCh <- err
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- s.waitForContainer(id)
	}()

	select {
	case <-abort:
		s.killContainer(id, waitCh)
		err = errors.New("Aborted")

	case err = <-attachCh:
		s.killContainer(id, waitCh)
		s.Debugln("Container", id, "finished with", err)

	case err = <-waitCh:
		s.Debugln("Container", id, "finished with", err)
	}
	return
}

func (s *executor) removeContainer(id string) error {
	s.disconnectNetwork(id)
	options := types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}
	err := s.client.ContainerRemove(context.TODO(), id, options)
	s.Debugln("Removed container", id, "with", err)
	return err
}

func (s *executor) disconnectNetwork(id string) error {
	netList, err := s.client.NetworkList(context.TODO(), types.NetworkListOptions{})
	if err != nil {
		s.Debugln("Can't get network list. ListNetworks exited with", err)
		return err
	}

	for _, network := range netList {
		for _, pluggedContainer := range network.Containers {
			if id == pluggedContainer.Name {
				err = s.client.NetworkDisconnect(context.TODO(), network.ID, id, true)
				if err != nil {
					s.Warningln("Can't disconnect possibly zombie container", pluggedContainer.Name, "from network", network.Name, "->", err)
				} else {
					s.Warningln("Possibly zombie container", pluggedContainer.Name, "is disconnected from network", network.Name)
				}
				break
			}
		}
	}
	return err
}

func (s *executor) verifyAllowedImage(image, optionName string, allowedImages []string, internalImages []string) error {
	for _, allowedImage := range allowedImages {
		ok, _ := filepath.Match(allowedImage, image)
		if ok {
			return nil
		}
	}

	for _, internalImage := range internalImages {
		if internalImage == image {
			return nil
		}
	}

	if len(allowedImages) != 0 {
		s.Println()
		s.Errorln("The", image, "is not present on list of allowed", optionName)
		for _, allowedImage := range allowedImages {
			s.Println("-", allowedImage)
		}
		s.Println()
	} else {
		// by default allow to override the image name
		return nil
	}

	s.Println("Please check runner's configuration: http://doc.gitlab.com/ci/docker/using_docker_images.html#overwrite-image-and-services")
	return errors.New("invalid image")
}

func (s *executor) getImageName() (string, error) {
	if s.options.Image != "" {
		image := s.Build.GetAllVariables().ExpandValue(s.options.Image)
		err := s.verifyAllowedImage(s.options.Image, "images", s.Config.Docker.AllowedImages, []string{s.Config.Docker.Image})
		if err != nil {
			return "", err
		}
		return image, nil
	}

	if s.Config.Docker.Image == "" {
		return "", errors.New("No Docker image specified to run the build in")
	}

	return s.Config.Docker.Image, nil
}

func (s *executor) connectDocker() (err error) {
	client, err := docker_helpers.New(s.Config.Docker.DockerCredentials, DockerAPIVersion)
	if err != nil {
		return err
	}
	s.client = client

	s.info, err = client.Info(context.TODO())
	if err != nil {
		return err
	}

	return
}

func (s *executor) createDependencies() (err error) {
	err = s.bindDevices()
	if err != nil {
		return err
	}

	s.Debugln("Creating build volume...")
	err = s.createBuildVolume()
	if err != nil {
		return err
	}

	s.Debugln("Creating services...")
	err = s.createServices()
	if err != nil {
		return err
	}

	s.Debugln("Creating user-defined volumes...")
	err = s.createUserVolumes()
	if err != nil {
		return err
	}

	return
}

func (s *executor) Prepare(globalConfig *common.Config, config *common.RunnerConfig, build *common.Build) error {
	err := s.prepareBuildsDir(config)
	if err != nil {
		return err
	}

	err = s.AbstractExecutor.Prepare(globalConfig, config, build)
	if err != nil {
		return err
	}

	if s.BuildShell.PassFile {
		return errors.New("Docker doesn't support shells that require script file")
	}

	if config.Docker == nil {
		return errors.New("Missing docker configuration")
	}

	err = build.Options.Decode(&s.options)
	if err != nil {
		return err
	}

	imageName, err := s.getImageName()
	if err != nil {
		return err
	}

	s.Println("Using Docker executor with image", imageName, "...")

	err = s.connectDocker()
	if err != nil {
		return err
	}

	err = s.createDependencies()
	if err != nil {
		return err
	}
	return nil
}

func (s *executor) prepareBuildsDir(config *common.RunnerConfig) error {
	rootDir := config.BuildsDir
	if rootDir == "" {
		rootDir = s.DefaultBuildsDir
	}
	if s.isHostMountedVolume(rootDir, config.Docker.Volumes...) {
		s.SharedBuildsDir = true
	}
	return nil
}

func (s *executor) Cleanup() {
	var wg sync.WaitGroup

	remove := func(id string) {
		wg.Add(1)
		go func() {
			s.removeContainer(id)
			wg.Done()
		}()
	}

	for _, failureID := range s.failures {
		remove(failureID)
	}

	for _, service := range s.services {
		remove(service.ID)
	}

	for _, cacheID := range s.caches {
		remove(cacheID)
	}

	for _, build := range s.builds {
		remove(build.ID)
	}

	wg.Wait()

	if s.client != nil {
		s.client.Close()
	}

	s.AbstractExecutor.Cleanup()
}

func (s *executor) runServiceHealthCheckContainer(service *types.Container, timeout time.Duration) error {
	waitImage, err := s.getPrebuiltImage()
	if err != nil {
		return err
	}

	containerName := service.Names[0] + "-wait-for-service"

	config := &container.Config{
		Cmd:    []string{"gitlab-runner-service"},
		Image:  waitImage.ID,
		Labels: s.getLabels("wait", "wait="+service.ID),
	}
	hostConfig := &container.HostConfig{
		RestartPolicy: neverRestartPolicy,
		Links:         []string{service.Names[0] + ":" + service.Names[0]},
		NetworkMode:   container.NetworkMode(s.Config.Docker.NetworkMode),
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}
	s.Debugln("Waiting for service container", containerName, "to be up and running...")
	resp, err := s.client.ContainerCreate(context.TODO(), config, hostConfig, nil, containerName)
	if err != nil {
		return err
	}
	defer s.removeContainer(resp.ID)
	err = s.client.ContainerStart(context.TODO(), resp.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- s.waitForContainer(resp.ID)
	}()

	// these are warnings and they don't make the build fail
	select {
	case err := <-waitResult:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("service %v did timeout", containerName)
	}
}

func (s *executor) waitForServiceContainer(service *types.Container, timeout time.Duration) error {
	err := s.runServiceHealthCheckContainer(service, timeout)
	if err == nil {
		return nil
	}

	var buffer bytes.Buffer
	buffer.WriteString("\n")
	buffer.WriteString(helpers.ANSI_YELLOW + "*** WARNING:" + helpers.ANSI_RESET + " Service " + service.Names[0] + " probably didn't start properly.\n")
	buffer.WriteString("\n")
	buffer.WriteString(strings.TrimSpace(err.Error()) + "\n")

	var containerBuffer bytes.Buffer

	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
	}

	hijacked, err := s.client.ContainerLogs(context.TODO(), service.ID, options)
	if err == nil {
		defer hijacked.Close()
		stdcopy.StdCopy(&containerBuffer, &containerBuffer, hijacked)
		if containerLog := containerBuffer.String(); containerLog != "" {
			buffer.WriteString("\n")
			buffer.WriteString(strings.TrimSpace(containerLog))
			buffer.WriteString("\n")
		}
	} else {
		buffer.WriteString(strings.TrimSpace(err.Error()) + "\n")
	}

	buffer.WriteString("\n")
	buffer.WriteString(helpers.ANSI_YELLOW + "*********" + helpers.ANSI_RESET + "\n")
	buffer.WriteString("\n")
	io.Copy(s.BuildTrace, &buffer)
	return err
}
