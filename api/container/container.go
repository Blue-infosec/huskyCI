package container

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/globocom/huskyCI/api/log"
	goContext "golang.org/x/net/context"
)

// Container holds all information regarding a container.
type Container struct {
	dockerClient *client.Client
	CID          string    `bson:"CID,omitempty" json:"CID"`
	Status       string    `bson:"status,omitempty" json:"status"`
	Command      string    `bson:"cmd" json:"cmd"`
	Output       string    `bson:"output,omitempty" json:"output"`
	Image        Image     `bson:"image" json:"image"`
	StartedAt    time.Time `bson:"startedAt,omitempty" json:"startedAt"`
	FinishedAt   time.Time `bson:"finishedAt,omitempty" json:"finishedAt"`
}

// Image is the struct that holds all information regarding a container image.
type Image struct {
	CanonicalURL string `bson:"canonicalURL" json:"canonicalURL"`
	Name         string `bson:"name" json:"name"`
	Tag          string `bson:"tag" json:"tag"`
}

// NewDockerClient creates a new docker API client and set it to the container struct.
func (c *Container) NewDockerClient() error {

	if err := setDockerClientEnvs(); err != nil {
		return err
	}

	newClient, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	c.dockerClient = newClient
	return nil
}

// Run runs a container by creating and starting it.
func (c *Container) Run(repositoryURL, branch string) error {

	// step 1: create a new docker client
	if err := c.NewDockerClient(); err != nil {
		log.Error("RUN", "CONTAINER", 3005, err)
		return err
	}

	// step 2: pull image if it is not there yet
	imageIsLoaded, err := c.ImageIsLoaded()
	if err != nil {
		return err
	}
	if !imageIsLoaded {
		if err := c.PullImageWorker(); err != nil {
			return err
		}
	}

	// step 3: create the container
	if err := c.Create(repositoryURL, branch); err != nil {
		log.Error("RUN", "CONTAINER", 3014, c.Image.Name, c.Image.Tag, err)
		return err
	}
	c.Status = "created"

	// step 4: start the container
	c.StartedAt = time.Now()
	if err := c.Start(); err != nil {
		c.Status = "finished"
		log.Error("RUN", "CONTAINER", 3015, c.Image.Name, c.Image.Tag, err)
		return err
	}
	log.Info("RUN", "CONTAINER", 32, c.Image.Name, c.Image.Tag)
	c.Status = "running"

	// step 5: wait the container finish
	if err := c.Wait(); err != nil {
		c.Status = "finished"
		log.Error("RUN", "CONTAINER", 3016, err)
		return err
	}

	// step 6: read container's STDOUT when it finishes
	c.FinishedAt = time.Now()
	if err := c.ReadOutput(true, false); err != nil {
		log.Error("RUN", "CONTAINER", 3007, err)
		return err
	}
	log.Info("RUN", "CONTAINER", 34, c.Image.Name, c.Image.Tag)
	c.Status = "finished"

	// step 7: remove container from docker API
	if err := c.Remove(); err != nil {
		log.Error("RUN", "CONTAINER", 3027, err)
	}

	return nil
}

// Create creates a new container, set its CID and return an error.
func (c *Container) Create(repositoryURL, branch string) error {

	ctx := goContext.Background()
	fullImageName := fmt.Sprintf("%s:%s", c.Image.Name, c.Image.Tag)

	// replace GIT repository URL, branch and SSH private key from os env var
	cmd := HandleCmd(repositoryURL, branch, c.Command)
	finalCMD := HandlePrivateSSHKey(cmd)

	containerConfig := &container.Config{
		Image: fullImageName,
		Tty:   true,
		Cmd:   []string{"/bin/sh", "-c", finalCMD},
	}

	resp, err := c.dockerClient.ContainerCreate(ctx, containerConfig, nil, nil, "")
	if err != nil {
		return err
	}

	c.CID = resp.ID
	return nil
}

// Start starts a container and returns its error.
func (c *Container) Start() error {

	ctx := goContext.Background()

	return c.dockerClient.ContainerStart(ctx, c.CID, dockerTypes.ContainerStartOptions{})
}

// Wait returns when container finishes executing cmd.
func (c *Container) Wait() error {

	ctx := goContext.Background()

	statusCode, err := c.dockerClient.ContainerWait(ctx, c.CID)
	if statusCode != 0 {
		log.Error("Wait", "CONTAINER", 3028, statusCode, err)
	}

	return err
}

// Stop stops an active container by it's CID.
func (c *Container) Stop() error {

	ctx := goContext.Background()

	return c.dockerClient.ContainerStop(ctx, c.CID, nil)
}

// Remove removes a container by it's CID.
func (c *Container) Remove() error {

	ctx := goContext.Background()

	return c.dockerClient.ContainerRemove(ctx, c.CID, dockerTypes.ContainerRemoveOptions{})
}

// PullImage pulls an image, like docker pull.
func (c *Container) PullImage() error {

	ctx := goContext.Background()

	_, err := c.dockerClient.ImagePull(ctx, c.Image.CanonicalURL, dockerTypes.ImagePullOptions{})

	return err
}

// PullImageWorker will try to pull the container image a few times before returning a error
func (c *Container) PullImageWorker() error {
	timeout := time.After(15 * time.Minute)
	retryTick := time.NewTicker(15 * time.Second)
	for {
		select {
		case <-timeout:

			timeOutErr := errors.New("timeout")
			log.Error("pullImageWorker", "HUSKYDOCKER", 3013, timeOutErr)

			return timeOutErr

		case <-retryTick.C:

			log.Info("pullImageWorker", "HUSKYDOCKER", 31, c.Image.Name)

			isLoaded, err := c.ImageIsLoaded()
			if err != nil {
				log.Error("pullImageWorker", "HUSKYDOCKER", 3029, err)
				return err
			}
			if isLoaded {
				log.Info("pullImageWorker", "HUSKYDOCKER", 35, c.Image.Name)
				return nil
			}

			if err := c.PullImage(); err != nil {
				log.Error("pullImageWorker", "HUSKYDOCKER", 3013, err)
				return err
			}
		}
	}
}

// ListImages returns docker images, like docker image ls.
func (c *Container) ListImages() ([]dockerTypes.ImageSummary, error) {

	ctx := goContext.Background()

	return c.dockerClient.ImageList(ctx, dockerTypes.ImageListOptions{})
}

// RemoveImage removes an image.
func (c *Container) RemoveImage(imageID string) ([]dockerTypes.ImageDelete, error) {

	ctx := goContext.Background()

	return c.dockerClient.ImageRemove(ctx, imageID, dockerTypes.ImageRemoveOptions{Force: true})
}

// ReadOutput returns the output of a container based on isSTDERR and isSTDOUT bool parameters.
func (c *Container) ReadOutput(isSTDOUT, isSTDERR bool) error {

	ctx := goContext.Background()
	containerLogOptions := dockerTypes.ContainerLogsOptions{
		ShowStdout: isSTDOUT,
		ShowStderr: isSTDERR,
	}

	cOutput, err := c.dockerClient.ContainerLogs(ctx, c.CID, containerLogOptions)
	if err != nil {
		return err
	}

	body, err := ioutil.ReadAll(cOutput)
	if err != nil {
		return err
	}

	c.Output = string(body)

	return nil
}

// ImageIsLoaded returns a bool if a a docker image is loaded in DockerAPI or not.
func (c *Container) ImageIsLoaded() (bool, error) {

	ctx := goContext.Background()

	fullImageName := fmt.Sprintf("%s:%s", c.Image.Name, c.Image.Tag)
	args := filters.NewArgs()
	args.Add("reference", fullImageName)
	options := dockerTypes.ImageListOptions{Filters: args}

	resultImageList, err := c.dockerClient.ImageList(ctx, options)
	if err != nil {
		return false, err
	}

	isLoaded := (len(resultImageList) != 0)
	return isLoaded, nil
}

// HealthCheckDockerAPI pings DockerAPI to check if it is up and running.
func HealthCheckDockerAPI() error {

	var healthCheckContainer Container

	ctx := goContext.Background()

	err := healthCheckContainer.NewDockerClient()
	if err != nil {
		log.Error("HealthCheckDockerAPI", "CONTAINER", 3011, err)
		return err
	}

	_, err = healthCheckContainer.dockerClient.Ping(ctx)
	return err
}

// setDockerClientEnvs sets env vars needed by docker/docker library to create a NewEnvClient.
func setDockerClientEnvs() error {

	dockerAPIAddress := os.Getenv("HUSKYCI_DOCKERAPI_ADDR")
	dockerAPIPort := os.Getenv("HUSKYCI_DOCKERAPI_PORT")
	if dockerAPIPort == "" {
		dockerAPIPort = "2376"
	}

	dockerHost := fmt.Sprintf("https://%s:%s", dockerAPIAddress, dockerAPIPort)
	pathCertificate := os.Getenv("HUSKYCI_DOCKERAPI_CERT_PATH")
	tlsVerify := os.Getenv("HUSKYCI_DOCKERAPI_TLS_VERIFY")

	if tlsVerify == "" {
		tlsVerify = "1"
	}

	// env vars needed by docker/docker library to create a NewEnvClient:
	if err := os.Setenv("DOCKER_HOST", dockerHost); err != nil {
		log.Error("setDockerClientEnvs", "CONTAINER", 3001, err)
		return err
	}

	if err := os.Setenv("DOCKER_CERT_PATH", pathCertificate); err != nil {
		log.Error("setDockerClientEnvs", "CONTAINER", 3019, err)
		return err
	}

	if err := os.Setenv("DOCKER_TLS_VERIFY", tlsVerify); err != nil {
		log.Error("setDockerClientEnvs", "CONTAINER", 3020, err)
		return err
	}

	return nil
}

// HandleCmd will extract %GIT_REPO% and %GIT_BRANCH% from cmd and replace it with the proper repository URL.
func HandleCmd(repositoryURL, repositoryBranch, cmd string) string {
	if repositoryURL != "" && repositoryBranch != "" && cmd != "" {
		replace1 := strings.Replace(cmd, "%GIT_REPO%", repositoryURL, -1)
		replace2 := strings.Replace(replace1, "%GIT_BRANCH%", repositoryBranch, -1)
		return replace2
	}
	return ""
}

// HandlePrivateSSHKey will extract %GIT_PRIVATE_SSH_KEY% from cmd and replace it with the proper private SSH key.
func HandlePrivateSSHKey(rawString string) string {
	privKey := os.Getenv("HUSKYCI_API_GIT_PRIVATE_SSH_KEY")
	cmdReplaced := strings.Replace(rawString, "GIT_PRIVATE_SSH_KEY", privKey, -1)
	return cmdReplaced
}
