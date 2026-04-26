package docker

import "github.com/docker/docker/client"

func (e *Environment) Client() *client.Client {
	return e.client
}
