/*
Copyright 2018 codestation

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

package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"megpoid.xyz/go/swarm-updater/log"
)

const serviceLabel string = "xyz.megpoid.swarm-updater"
const updateOnlyLabel string = "xyz.megpoid.swarm-updater.update-only"
const enabledServiceLabel string = "xyz.megpoid.swarm-updater.enable"

// Swarm struct to handle all the service operations
type Swarm struct {
	client      DockerClient
	Blacklist   []*regexp.Regexp
	LabelEnable bool
}

func (c *Swarm) validService(service swarm.Service) bool {
	if c.LabelEnable {
		label := service.Spec.Labels[enabledServiceLabel]
		return strings.ToLower(label) == "true"
	}

	serviceName := service.Spec.Name

	for _, entry := range c.Blacklist {
		if entry.MatchString(serviceName) {
			return false
		}
	}

	return true
}

// NewSwarm instantiates a new Docker swarm client
func NewSwarm() (*Swarm, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize docker client")
	}

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create docker cli")
	}

	if err = dockerCli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, errors.Wrap(err, "failed to initialize docker cli")
	}

	return &Swarm{client: &dockerClient{apiClient: cli, dockerCli: dockerCli}}, nil
}

func (c *Swarm) serviceList(ctx context.Context) ([]swarm.Service, error) {
	services, err := c.client.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "ServiceList failed")
	}

	return services, nil
}

func (c *Swarm) updateService(ctx context.Context, service swarm.Service) error {
	image := service.Spec.TaskTemplate.ContainerSpec.Image
	updateOpts := types.ServiceUpdateOptions{}

	// get docker auth
	encodedAuth, err := c.client.RetrieveAuthTokenFromImage(ctx, image)
	if err != nil {
		return errors.Wrap(err, "cannot retrieve auth token from service's image")
	}

	// do not set auth if is an empty json object
	if encodedAuth != "e30=" {
		updateOpts.EncodedRegistryAuth = encodedAuth
	}

	// remove image hash from name
	imageName := strings.Split(image, "@sha")[0]

	// fetch a newer image digest
	service.Spec.TaskTemplate.ContainerSpec.Image, err = c.getImageDigest(ctx, imageName, updateOpts.EncodedRegistryAuth)
	if err != nil {
		return errors.Wrap(err, "failed to get new image digest")
	}

	if image == service.Spec.TaskTemplate.ContainerSpec.Image {
		log.Debug("Service %s is already up to date", service.Spec.Name)
		return nil
	}

	if strings.ToLower(service.Spec.Labels[updateOnlyLabel]) == "true" {
		if service.Spec.Mode.Replicated != nil && service.Spec.Mode.Replicated.Replicas != nil {
			*service.Spec.Mode.Replicated.Replicas = 0
		}
	}

	log.Debug("Updating service %s...", service.Spec.Name)
	response, err := c.client.ServiceUpdate(ctx, service.ID, service.Version, service.Spec, updateOpts)
	if err != nil {
		return errors.Wrapf(err, "failed to update service %s", service.Spec.Name)
	}

	for _, warning := range response.Warnings {
		log.Debug("response warning:\n%s", warning)
	}

	updatedService, _, err := c.client.ServiceInspectWithRaw(ctx, service.ID, types.ServiceInspectOptions{})
	if err != nil {
		return errors.Wrapf(err, "cannot inspect service %s to check update status", service.Spec.Name)
	}

	previous := updatedService.PreviousSpec.TaskTemplate.ContainerSpec.Image
	current := updatedService.Spec.TaskTemplate.ContainerSpec.Image

	if previous != current {
		log.Printf("Service %s updated to %s", service.Spec.Name, current)
	} else {
		log.Debug("Service %s is up to date", service.Spec.Name)
	}

	return nil
}

// UpdateServices updates all the services from a Docker swarm
func (c *Swarm) UpdateServices(ctx context.Context) error {
	services, err := c.serviceList(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get service list")
	}

	var serviceID string

	for _, service := range services {
		if c.validService(service) {

			// try to identify this service
			if _, ok := service.Spec.Annotations.Labels[serviceLabel]; ok {
				serviceID = service.ID
				continue
			}

			if err = c.updateService(ctx, service); err != nil {
				if ctx.Err() == context.Canceled {
					log.Printf("Service update canceled")
					break
				}
				log.Printf("Cannot update service %s: %s", service.Spec.Name, err.Error())
			}
		} else {
			log.Debug("Service %s was ignored by blacklist or missing label", service.Spec.Name)
		}
	}

	if serviceID != "" {
		// refresh service
		service, _, err := c.client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
		if err != nil {
			return errors.Wrapf(err, "cannot inspect the service %s", serviceID)
		}

		err = c.updateService(ctx, service)
		if err != nil {
			return errors.Wrapf(err, "failed to update the service %s", serviceID)
		}
	}

	return nil
}

func (c *Swarm) getImageDigest(ctx context.Context, image, encodedAuth string) (string, error) {
	namedRef, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse image name")
	}

	if _, isCanonical := namedRef.(reference.Canonical); isCanonical {
		return "", errors.New("the image name already have a digest")
	}

	distributionInspect, err := c.client.DistributionInspect(ctx, image, encodedAuth)
	if err != nil {
		return "", errors.Wrap(err, "failed to inspect image")
	}

	// ensure that image gets a default tag if none is provided
	img, err := reference.WithDigest(namedRef, distributionInspect.Descriptor.Digest)
	if err != nil {
		return "", errors.Wrap(err, "the image name has an invalid format")
	}

	return reference.FamiliarString(img), nil
}
