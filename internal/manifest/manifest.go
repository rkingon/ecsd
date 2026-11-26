// Package manifest is the stage manifest: where a deploy runs. Whatever builds the infrastructure (Terraform, CDK,
// CloudFormation, Pulumi, a person) writes it, as JSON, to an SSM parameter; ecsd only reads it.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Manifest struct {
	Cluster    string `json:"cluster" jsonschema_description:"Cluster name or ARN."`
	Region     string `json:"region,omitempty" jsonschema_description:"Checked against the credentials' region when set."`
	Registry   string `json:"registry,omitempty" jsonschema_description:"Fills ${registry} in ecsd.yml's images."`
	LaunchType string `json:"launch_type,omitempty" jsonschema:"enum=FARGATE,enum=EC2,enum=EXTERNAL" jsonschema_description:"For one-off tasks (release, run, console). Default FARGATE; ignored when capacity_provider_strategy is set."`

	CapacityProviderStrategy []CapacityProvider `json:"capacity_provider_strategy,omitempty"`

	Services map[string]Family `json:"services" jsonschema_description:"Services ecsd rolls, by name. Each must exist, and ignore task definition changes in whatever manages it."`
	Tasks    map[string]Family `json:"tasks,omitempty" jsonschema_description:"Task families ecsd registers on every deploy without a service: one-off tasks, consoles, tasks an app starts itself."`
	Network  *Network          `json:"network,omitempty" jsonschema_description:"For one-off tasks. Defaults to the first service's."`
	Hosts    map[string]string `json:"hosts,omitempty" jsonschema_description:"Base URLs or host names, substituted as written for ${hosts.<name>} in health and smoke checks."`

	ReleaseParameter string `json:"release_parameter,omitempty" jsonschema_description:"Where releases are recorded. Default /<app>/<stage>/release."`
}

type Family struct {
	Template string   `json:"template" jsonschema_description:"Task definition ARN (or family:revision) every deploy copies, changing only the image."`
	Service  string   `json:"service,omitempty" jsonschema_description:"Services only: the ECS service's name, when it isn't the key."`
	Network  *Network `json:"network,omitempty" jsonschema_description:"Tasks only: overrides the manifest's network."`
}

type Network struct {
	Subnets        []string `json:"subnets"`
	SecurityGroups []string `json:"security_groups,omitempty"`
	AssignPublicIP bool     `json:"assign_public_ip,omitempty"`
}

type CapacityProvider struct {
	CapacityProvider string `json:"capacity_provider"`
	Weight           int32  `json:"weight,omitempty"`
	Base             int32  `json:"base,omitempty"`
}

func Parse(b []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

func (m *Manifest) Validate() error {
	var errs []error
	if m.Cluster == "" {
		errs = append(errs, errors.New("cluster: empty"))
	}
	if len(m.Services) == 0 && len(m.Tasks) == 0 {
		errs = append(errs, errors.New("services and tasks: none"))
	}
	for name, f := range m.Services {
		if f.Template == "" {
			errs = append(errs, fmt.Errorf("services.%s.template: empty", name))
		}
		if f.Network != nil {
			errs = append(errs, fmt.Errorf("services.%s.network: a service's network is the service's own", name))
		}
		if _, dup := m.Tasks[name]; dup {
			errs = append(errs, fmt.Errorf("%s: both a service and a task", name))
		}
	}
	for name, f := range m.Tasks {
		if f.Template == "" {
			errs = append(errs, fmt.Errorf("tasks.%s.template: empty", name))
		}
		if f.Service != "" {
			errs = append(errs, fmt.Errorf("tasks.%s.service: tasks have no service", name))
		}
	}
	for _, n := range m.networks() {
		if len(n.Subnets) == 0 {
			errs = append(errs, errors.New("network: no subnets"))
		}
	}
	return errors.Join(errs...)
}

func (m *Manifest) networks() []*Network {
	var ns []*Network
	if m.Network != nil {
		ns = append(ns, m.Network)
	}
	for _, f := range m.Tasks {
		if f.Network != nil {
			ns = append(ns, f.Network)
		}
	}
	return ns
}

// Family returns the named service or task, and whether it's a service.
func (m *Manifest) Family(name string) (Family, bool, bool) {
	if f, ok := m.Services[name]; ok {
		return f, true, true
	}
	f, ok := m.Tasks[name]
	return f, false, ok
}

// ServiceName is the ECS service a services entry rolls.
func (m *Manifest) ServiceName(name string) string {
	if s := m.Services[name].Service; s != "" {
		return s
	}
	return name
}

// Names is every service then every task, each sorted, so output and registration order are stable.
func (m *Manifest) Names() []string {
	return append(sortedKeys(m.Services), sortedKeys(m.Tasks)...)
}

func (m *Manifest) ServiceNames() []string { return sortedKeys(m.Services) }

func (m *Manifest) TaskNames() []string { return sortedKeys(m.Tasks) }

func sortedKeys(fs map[string]Family) []string {
	ks := make([]string, 0, len(fs))
	for k := range fs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Host resolves ${hosts.<name>}.
func (m *Manifest) Host(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, "hosts.")
	if !ok {
		return "", false
	}
	v, ok := m.Hosts[name]
	return strings.TrimSuffix(v, "/"), ok
}
