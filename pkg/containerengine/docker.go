// Copyright Nitric Pty Ltd.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package containerengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/containers/podman/v3/pkg/domain/entities"
	"github.com/containers/podman/v3/pkg/specgen"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/jhoonb/archivex"
	"github.com/pkg/errors"
)

type docker struct {
	cli *client.Client
}

var _ ContainerEngine = &docker{}

func newDocker() (ContainerEngine, error) {
	cmd := exec.Command("docker", "--version")
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	cmd = exec.Command("systemctl", "is-active", "docker")
	err = cmd.Run()
	if err != nil || cmd.ProcessState.ExitCode() != 0 {
		fmt.Println("docker service not running, starting..")
		cmd = exec.Command("systemctl", "start", "docker")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			return nil, err
		}
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}

	return &docker{cli: cli}, err
}

func (d *docker) Build(dockerfile, srcPath, imageTag, provider string, buildArgs map[string]string) error {
	buildArgs["PROVIDER"] = provider

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout())
	defer cancel()

	tar := new(archivex.TarFile)
	dockerBuildContext := bytes.Buffer{}
	err := tar.CreateWriter("src.tar", &dockerBuildContext)
	if err != nil {
		return err
	}
	err = tar.AddAll(srcPath, false)
	if err != nil {
		return err
	}
	if strings.Contains(dockerfile, "/tmp") {
		// copy the generated dockerfile into the tar.
		df, err := os.Open(dockerfile)
		if err != nil {
			return err
		}
		s, err := os.Stat(dockerfile)
		if err != nil {
			return err
		}
		err = tar.Add(s.Name(), df, s)
		if err != nil {
			return err
		}
		dockerfile = s.Name()
	}
	tar.Close()
	opts := types.ImageBuildOptions{
		SuppressOutput: false,
		Dockerfile:     dockerfile,
		Tags:           []string{imageTag},
		Remove:         true,
		ForceRemove:    true,
		PullParent:     true,
	}
	res, err := d.cli.ImageBuild(ctx, &dockerBuildContext, opts)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	return print(res.Body)
}

type ErrorLine struct {
	Error       string      `json:"error"`
	ErrorDetail ErrorDetail `json:"errorDetail"`
}

type ErrorDetail struct {
	Message string `json:"message"`
}

type Line struct {
	Stream string `json:"stream"`
}

func print(rd io.Reader) error {
	var lastLine string

	scanner := bufio.NewScanner(rd)
	for scanner.Scan() {
		lastLine = scanner.Text()
		line := &Line{}
		json.Unmarshal([]byte(lastLine), line)
		if len(line.Stream) > 0 {
			fmt.Print(line.Stream)
		}
	}

	errLine := &ErrorLine{}
	json.Unmarshal([]byte(lastLine), errLine)
	if errLine.Error != "" {
		return errors.New(errLine.Error)
	}

	return scanner.Err()
}

func (d *docker) ListImages(stackName, containerName string) ([]Image, error) {
	opts := types.ImageListOptions{}
	opts.Filters.Add("reference", fmt.Sprintf("localhost/%s-%s-*", stackName, containerName))
	imageSummaries, err := d.cli.ImageList(context.Background(), opts)
	if err != nil {
		return nil, err
	}

	imgs := []Image{}
	for _, i := range imageSummaries {
		nameParts := strings.Split(i.ID, ":")
		imgs = append(imgs, Image{
			ID:         i.ID,
			Repository: nameParts[0],
			Tag:        nameParts[1],
			CreatedAt:  time.Unix(i.Created, 0).Local().String(),
		})
	}
	return imgs, err
}

func (d *docker) Pull(rawImage string) error {
	_, err := d.cli.ImagePull(context.Background(), rawImage, types.ImagePullOptions{})
	return err
}

func (d *docker) NetworkCreate(name string) error {
	_, err := d.cli.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	if err == nil {
		// it already exists, no need to create.
		return nil
	}
	_, err = d.cli.NetworkCreate(context.Background(), name, types.NetworkCreate{})
	return err
}

func (d *docker) ContainerCreate(name string) error {
	config := container.Config{}

	_, err := d.cli.ContainerCreate(context.Background(), &config, nil, nil, nil, name)
	return err
}

func createArgsFromSpec(s *specgen.SpecGenerator) (*container.Config, *container.HostConfig, *network.NetworkingConfig, error) {
	exposedPorts := nat.PortSet{}
	for i, p := range s.Expose {
		port, err := nat.NewPort(p, fmt.Sprintf("%d", i))
		if err != nil {
			return nil, nil, nil, errors.WithMessage(err, "newport")
		}
		exposedPorts[port] = struct{}{}
	}
	env := []string{}
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	mounts := []mount.Mount{}
	vols := map[string]struct{}{}
	for _, m := range s.Mounts {
		mounts = append(mounts, mount.Mount{
			Source: m.Source,
			Type:   mount.Type(m.Type),
			Target: m.Destination,
		})
		vols[m.Destination] = struct{}{}
	}

	config := &container.Config{
		Image:        s.Image,
		Labels:       s.Labels,
		Cmd:          s.Command,
		Env:          env,
		ExposedPorts: exposedPorts,
		Volumes:      vols,
		Hostname:     s.Hostname,
		User:         s.User,
	}
	pb := nat.PortMap{}
	for _, pm := range s.PortMappings {
		port, err := nat.NewPort(pm.Protocol, fmt.Sprintf("%d", pm.ContainerPort))
		if err != nil {
			return nil, nil, nil, errors.WithMessage(err, "newport")
		}
		pb[port] = []nat.PortBinding{
			{
				HostIP:   pm.HostIP,
				HostPort: fmt.Sprintf("%d/%s", pm.HostPort, pm.Protocol),
			},
		}
	}

	hostConfig := &container.HostConfig{
		PortBindings: pb,
		Mounts:       mounts,
	}
	if len(s.ContainerNetworkConfig.CNINetworks) > 0 {
		hostConfig.NetworkMode = container.NetworkMode(s.ContainerNetworkConfig.CNINetworks[0])
	}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}
	for key, alias := range s.ContainerNetworkConfig.Aliases {
		networkingConfig.EndpointsConfig[key] = &network.EndpointSettings{
			Aliases: alias,
		}
	}

	return config, hostConfig, networkingConfig, nil
}

func (d *docker) CreateWithSpec(s *specgen.SpecGenerator) (string, error) {
	config, hostConfig, networkingConfig, err := createArgsFromSpec(s)
	if err != nil {
		return "", errors.WithMessage(err, "ContainerArgs")
	}

	resp, err := d.cli.ContainerCreate(context.Background(), config, hostConfig, networkingConfig, nil, s.Name)
	if err != nil {
		return "", errors.WithMessage(err, "ContainerCreate")
	}
	return resp.ID, nil
}

func (d *docker) Start(nameOrID string) error {
	return d.cli.ContainerStart(context.Background(), nameOrID, types.ContainerStartOptions{})
}

func (d *docker) CopyFromArchive(nameOrID string, path string, reader io.Reader) error {
	return d.cli.CopyToContainer(context.Background(), nameOrID, path, reader, types.CopyToContainerOptions{})
}

func (d *docker) ContainersListByLabel(match map[string]string) ([]entities.ListContainer, error) {
	opts := types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(),
	}
	for k, v := range match {
		opts.Filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}

	res, err := d.cli.ContainerList(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	cons := []entities.ListContainer{}
	for _, con := range res {
		pmList := []ocicni.PortMapping{}
		for _, p := range con.Ports {
			pmList = append(pmList, ocicni.PortMapping{
				HostPort: int32(p.PublicPort),
			})
		}
		cons = append(cons, entities.ListContainer{
			ID:      con.ID,
			Command: []string{con.Command},
			Image:   con.Image,
			Status:  con.Status,
			State:   con.State,
			Labels:  con.Labels,
			Ports:   pmList,
		})
	}
	return cons, err
}

func (d *docker) RemoveByLabel(name, value string) error {
	opts := types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(),
	}
	opts.Filters.Add("label", fmt.Sprintf("%s=%s", name, value))

	res, err := d.cli.ContainerList(context.Background(), opts)
	if err != nil {
		return err
	}
	for _, con := range res {
		err = d.cli.ContainerRemove(context.Background(), con.ID, types.ContainerRemoveOptions{Force: true})
		if err != nil {
			return err
		}
	}
	return nil
}
