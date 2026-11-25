// Package schema generates the JSON Schemas of ecsd.yml and the stage manifest from the types ecsd parses them into,
// so the published schemas can't drift from the parser.
package schema

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"

	"github.com/rkingon/ecsd/internal/config"
	"github.com/rkingon/ecsd/internal/manifest"
)

const base = "https://rkingon.github.io/ecsd/schema/v1/"

// Names are the published files, without .json.
var Names = []string{"config", "manifest"}

func Generate(name string) ([]byte, error) {
	r := &jsonschema.Reflector{DoNotReference: true, AllowAdditionalProperties: false}
	var s *jsonschema.Schema
	switch name {
	case "config":
		r.FieldNameTag = "yaml"
		s = r.Reflect(&config.File{})
		s.Title = "ecsd.yml"
		s.Description = "What ecsd deploys: images, gates, the release task, health and smoke checks."
	case "manifest":
		s = r.Reflect(&manifest.Manifest{})
		s.Title = "ecsd stage manifest"
		s.Description = "Where ecsd deploys, written by the infrastructure to an SSM parameter (default /<app>/<stage>/deploy)."
	default:
		return nil, fmt.Errorf("no schema %q: want config or manifest", name)
	}
	s.ID = jsonschema.ID(base + name + ".json")
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
