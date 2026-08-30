package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/elements"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

type optionalDependencyFlags []string

func (values *optionalDependencyFlags) String() string { return strings.Join(*values, ",") }

func (values *optionalDependencyFlags) Set(value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return errors.New("optional dependency name must be canonical")
	}
	*values = append(*values, value)
	return nil
}

// runGraphPreflight is intentionally narrower than serve. It accepts only
// the exact executable catalog compiled into this binary, constructs an
// immutable config.Plan, reduces the broad catalog to that plan, and prints a
// public sealed preparation without mounting resources. Built-in schemas are
// resolved from their exact standard catalog; missing provider plugins,
// services, plugin schemas, or secret providers fail closed.
func runGraphPreflight(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("graph preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	lockPath := flags.String("lock", "", "resolution lock (default: openrealtime.lock beside graph)")
	valuesPath := flags.String("values", "", "strict values artifact (default: agent.values.yaml beside graph)")
	channelsPath := flags.String("channels", "", "optional strict channel-depth overlay")
	deploymentPath := flags.String("deployment", "", "deployment artifact (default: agent.deployment.yaml beside graph)")
	secretsPath := flags.String("secrets", "", "optional graph-scoped secret-reference catalog")
	outputPath := flags.String("out", "", "write public sealed preparation atomically instead of stdout")
	var optionalDependencies optionalDependencyFlags
	flags.Var(&optionalDependencies, "optional-dependency", "select one descriptor-declared optional service; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("graph preflight needs exactly one topology path")
	}
	topologyPath := flags.Args()[0]
	directory := filepath.Dir(topologyPath)
	if *lockPath == "" {
		*lockPath = filepath.Join(directory, "openrealtime.lock")
	}
	if *valuesPath == "" {
		*valuesPath = filepath.Join(directory, "agent.values.yaml")
	}
	if *deploymentPath == "" {
		*deploymentPath = filepath.Join(directory, "agent.deployment.yaml")
	}

	descriptors, err := elements.Catalog()
	if err != nil {
		return fmt.Errorf("graph preflight descriptor catalog: %w", err)
	}
	executableCatalog, err := elements.AssemblyCatalog()
	if err != nil {
		return fmt.Errorf("graph preflight executable catalog: %w", err)
	}
	discovery, err := executableCatalog.Discovery()
	if err != nil {
		return fmt.Errorf("graph preflight discovery: %w", err)
	}
	configSchemas, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		return fmt.Errorf("graph preflight config schemas: %w", err)
	}

	artifacts := graphconfig.Artifacts{}
	if artifacts.Topology, err = readConfigArtifact(topologyPath); err != nil {
		return err
	}
	if artifacts.Values, err = readConfigArtifact(*valuesPath); err != nil {
		return err
	}
	if artifacts.Lock, err = readConfigArtifact(*lockPath); err != nil {
		return err
	}
	if artifacts.Deployment, err = readConfigArtifact(*deploymentPath); err != nil {
		return err
	}
	if *channelsPath != "" {
		if artifacts.Channels, err = readConfigArtifact(*channelsPath); err != nil {
			return err
		}
	}

	var secretCatalog *graphsecret.Document
	if *secretsPath != "" {
		document, loadErr := loadGraphSecrets(*secretsPath)
		if loadErr != nil {
			return loadErr
		}
		secretCatalog = &document
	}
	plan, err := graphconfig.Create(context.Background(), artifacts, graphconfig.Options{
		Catalog: descriptors, Discovery: discovery, SecretCatalog: secretCatalog,
		Loader: graphcompiler.FileLoader{}, SchemaResolver: configSchemas,
		OptionalDependencies: []string(optionalDependencies),
	})
	if err != nil {
		return fmt.Errorf("graph preflight plan: %w", err)
	}
	selected, err := executableCatalog.Select(plan, secretCatalog)
	if err != nil {
		return err
	}
	prepared, err := graphassembly.Preflight(context.Background(), plan, selected)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(prepared.Public(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode graph preflight: %w", err)
	}
	return writeOrPrint(*outputPath, append(payload, '\n'), stdout)
}

func runGraphInventory(arguments []string, stdout io.Writer) error {
	if len(arguments) != 0 {
		return errors.New("graph inventory does not accept arguments")
	}
	inventory, err := elements.Inventory()
	if err != nil {
		return fmt.Errorf("graph assembly inventory: %w", err)
	}
	payload, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return fmt.Errorf("encode graph assembly inventory: %w", err)
	}
	_, err = stdout.Write(append(payload, '\n'))
	return err
}

func readConfigArtifact(path string) (graphconfig.Artifact, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return graphconfig.Artifact{}, fmt.Errorf("read graph artifact %s: %w", path, err)
	}
	return graphconfig.Artifact{Path: path, Data: payload}, nil
}
