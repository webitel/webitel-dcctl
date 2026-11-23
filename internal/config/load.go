// Package config loads and validates the configuration files of
// "webitel-dcctl serve" and "webitel-dcctl arbiter".
//
// Keys missing from a file take their defaults; unknown keys are an error.
package config

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

var errEmpty = errors.New("config is empty")

// decode reads a single YAML document into dst, which already holds the defaults.
func decode(data []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmpty
		}

		return err
	}

	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return errors.New("config must contain a single YAML document")
	}

	return nil
}
