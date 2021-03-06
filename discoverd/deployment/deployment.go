package deployment

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/attempt"
	"github.com/flynn/flynn/pkg/stream"
)

type DeploymentState string

const (
	DeploymentStatePerforming DeploymentState = "performing"
	DeploymentStateDone       DeploymentState = "done"
)

func NewDeploymentMeta(id string) DeploymentMeta {
	meta := DeploymentMeta{ID: id}
	meta.States = make(map[string]DeploymentState)
	return meta
}

type DeploymentMeta struct {
	ID     string                     `json:"id"`
	States map[string]DeploymentState `json:"states"`
}

func NewDeployment(service string) (*Deployment, error) {
	s := discoverd.NewService(service)
	events := make(chan *discoverd.Event)
	stream, err := s.Watch(events)
	if err != nil {
		return nil, err
	}
	return &Deployment{s, events, stream}, nil
}

// Deployment is a wrapper around service metadata for marking jobs as either
// performing a deployment or done deploying
type Deployment struct {
	service discoverd.Service
	events  chan *discoverd.Event
	stream  stream.Stream
}

// MarkPerforming marks the given address as performing in the service metadata,
// ensuring there is only one address marked as performing at any given time
// (waiting for a ServiceMeta event and retrying if there is already an
// address performing).
func (d *Deployment) MarkPerforming(addr string, timeout int) error {
outer:
	for {
		meta, err := d.service.GetMeta()
		if err != nil {
			return err
		}

		deploymentMeta, err := d.decode(meta)
		if err != nil {
			return err
		}

		performing := ""
		for a, state := range deploymentMeta.States {
			if a == addr {
				// already marked as performing, nothing to do
				return nil
			}

			if state == DeploymentStatePerforming {
				performing = a
				break
			}
		}

		// if another address is performing, wait for a ServiceMeta
		// event then try again
		if performing != "" {
			for {
				select {
				case event, ok := <-d.events:
					if !ok {
						return fmt.Errorf("service stream closed unexpectedly: %s", d.stream.Err())
					}
					if event.Kind == discoverd.EventKindServiceMeta {
						continue outer
					}
				case <-time.After(time.Duration(timeout) * time.Second):
					return fmt.Errorf("timed out waiting for %s to finish performing", performing)
				}
			}
		}

		// no performing addresses, attempt to mark addr
		deploymentMeta.States[addr] = DeploymentStatePerforming

		data, err := json.Marshal(deploymentMeta)
		if err != nil {
			return err
		}
		meta.Data = data

		if err := d.service.SetMeta(meta); err == nil {
			return nil
		}
	}
	return nil
}

var attempts = attempt.Strategy{
	Delay: 100 * time.Millisecond,
	Total: 30 * time.Second,
}

// MarkDone marks the addr as done in the service metadata
func (d *Deployment) MarkDone(addr string) error {
	return attempts.Run(func() error {
		meta, err := d.service.GetMeta()
		if err != nil {
			return err
		}

		deploymentMeta, err := d.decode(meta)
		if err != nil {
			return err
		}

		deploymentMeta.States[addr] = DeploymentStateDone

		return d.set(meta, deploymentMeta)
	})
}

// Wait waits for an expected number of "done" addresses in the service metadata
func (d *Deployment) Wait(id string, expected int, timeout int, log log15.Logger) error {
	for {
		actual := 0
		select {
		case event, ok := <-d.events:
			if !ok {
				return fmt.Errorf("service stream closed unexpectedly: %s", d.stream.Err())
			}
			if event.Kind == discoverd.EventKindServiceMeta {
				deploymentMeta, err := d.decode(event.ServiceMeta)
				if err != nil {
					return err
				}
				log.Info("got service meta event", "state", deploymentMeta)
				if deploymentMeta.ID == id {
					actual = 0
					for _, state := range deploymentMeta.States {
						if state == DeploymentStateDone {
							actual++
						}
					}
					if actual == expected {
						return nil
					}
				} else {
					log.Warn("ignoring service meta even with wrong ID", "expected", id, "got", deploymentMeta.ID)
				}

			}
		case <-time.After(time.Duration(timeout) * time.Second):
			return fmt.Errorf("timed out waiting for discoverd deployment (expected=%d actual=%d)", expected, actual)
		}
	}
}

// Create starts a new deployment with a given ID
func (d *Deployment) Create(id string) error {
	return attempts.Run(func() error {
		if err := d.set(&discoverd.ServiceMeta{}, NewDeploymentMeta(id)); err == nil {
			return nil
		}
		meta, err := d.service.GetMeta()
		if err != nil {
			return err
		}
		return d.set(meta, NewDeploymentMeta(id))
	})
}

func (d *Deployment) Close() error {
	return d.stream.Close()
}

func (d *Deployment) decode(meta *discoverd.ServiceMeta) (DeploymentMeta, error) {
	var deploymentMeta DeploymentMeta
	if err := json.Unmarshal(meta.Data, &deploymentMeta); err != nil {
		return deploymentMeta, err
	}
	return deploymentMeta, nil
}

// set updates the service metadata given the current version and the updated version
// will return err if the current version is no longer current (performs CaS)
func (d *Deployment) set(meta *discoverd.ServiceMeta, deploymentMeta DeploymentMeta) error {
	data, err := json.Marshal(deploymentMeta)
	if err != nil {
		return err
	}
	meta.Data = data
	return d.service.SetMeta(meta)
}
