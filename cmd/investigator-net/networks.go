package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// dropNetwork removes the network name from Lima's networks.yaml, keeping
// the other networks and the comments. Lima kills investigator-net on
// limactl stop but leaves networks.yaml alone; if the entry stayed, a
// hand-typed limactl start would bring up Lima's own, unfiltered usernet
// daemon on it (prototype-plan.md, step 6). A missing file or entry is fine.
func dropNetwork(path, name string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: not a mapping", path)
	}
	top := doc.Content[0]
	var nets *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "networks" {
			nets = top.Content[i+1]
		}
	}
	if nets == nil || nets.Kind != yaml.MappingNode {
		return nil
	}
	found := false
	for i := 0; i+1 < len(nets.Content); i += 2 {
		if nets.Content[i].Value == name {
			nets.Content = append(nets.Content[:i], nets.Content[i+2:]...)
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	if len(nets.Content) == 0 {
		nets.Style = yaml.FlowStyle // networks: {}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".networks-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
