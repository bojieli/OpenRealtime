package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/element/codec"
	"github.com/bojieli/OpenRealtime/elements"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

const graphUsage = `usage: openrealtime graph <command> [flags] <graph>

commands:
  fmt        canonicalize one or more .ortg files
  normalize  convert .ortg/YAML/JSON topology to normalized YAML or JSON
  update     deliberately resolve latest descriptors and write the lock
  check      compile with the lock and report validation findings
  compile    compile with the lock and write canonical Graph IR
  render     compile with the lock and generate Mermaid or DOT

topology contains only nodes, edges, boundaries, and ->/=> delivery. Element
values, deployment, secrets, evidence profiles, and rare depth overrides are
separate artifacts.`

func runGraph(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(graphUsage)
	}
	switch arguments[0] {
	case "fmt":
		return runGraphFmt(arguments[1:], stdout)
	case "normalize":
		return runGraphNormalize(arguments[1:], stdout)
	case "update":
		return runGraphCompile(arguments[1:], stdout, stderr, true, false, false)
	case "check":
		return runGraphCompile(arguments[1:], stdout, stderr, false, false, false)
	case "compile":
		return runGraphCompile(arguments[1:], stdout, stderr, false, true, false)
	case "render":
		return runGraphCompile(arguments[1:], stdout, stderr, false, false, true)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, graphUsage)
		return nil
	default:
		return fmt.Errorf("unknown graph command %q\n%s", arguments[0], graphUsage)
	}
}

func runGraphFmt(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("graph fmt", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	write := flags.Bool("w", false, "write canonical source back to each file")
	check := flags.Bool("check", false, "fail if source is not canonical")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(flags.Args()) == 0 {
		return errors.New("graph fmt needs at least one .ortg path")
	}
	if *write && *check {
		return errors.New("graph fmt -w and -check are mutually exclusive")
	}
	for _, path := range flags.Args() {
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read graph %s: %w", path, err)
		}
		file, err := syntax.Parse(path, body)
		if err != nil {
			return err
		}
		formatted := []byte(syntax.Format(file))
		if *check && string(body) != string(formatted) {
			return fmt.Errorf("graph %s is not canonically formatted", path)
		}
		if *write {
			if err := atomicWrite(path, formatted); err != nil {
				return err
			}
		} else if len(flags.Args()) == 1 {
			if _, err := stdout.Write(formatted); err != nil {
				return err
			}
		}
	}
	return nil
}

func runGraphNormalize(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("graph normalize", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	format := flags.String("format", "yaml", "normalized output: yaml or json")
	out := flags.String("out", "", "write output atomically instead of stdout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("graph normalize needs exactly one topology path")
	}
	file, err := loadGraphTopology(flags.Args()[0])
	if err != nil {
		return err
	}
	document := manifest.FromSyntax(file)
	var payload []byte
	switch strings.ToLower(*format) {
	case "yaml", "yml":
		payload, err = manifest.MarshalYAML(document)
	case "json":
		payload, err = manifest.MarshalJSON(document)
	default:
		return fmt.Errorf("graph normalize format must be yaml or json, got %q", *format)
	}
	if err != nil {
		return err
	}
	return writeOrPrint(*out, payload, stdout)
}

type stringFlags []string

func (values *stringFlags) String() string { return strings.Join(*values, ",") }
func (values *stringFlags) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("descriptor path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func runGraphCompile(
	arguments []string,
	stdout, stderr io.Writer,
	update, emitIR, render bool,
) error {
	flags := flag.NewFlagSet("graph", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	lockPath := flags.String("lock", "", "resolution lock path (default: openrealtime.lock beside graph)")
	output := flags.String("out", "", "output path (compile/render only)")
	profileName := flags.String("profile", "realtime-agent", "validation profile: core, realtime-agent, conversational-voice, computer-use")
	warningsAsErrors := flags.Bool("warnings-as-errors", false, "fail when the selected lint profile reports a warning")
	renderFormat := flags.String("format", "mermaid", "render output: mermaid or dot")
	valuesPath := flags.String("values", "", "separate strict YAML/JSON element-values artifact")
	var descriptorPaths stringFlags
	flags.Var(&descriptorPaths, "descriptor", "element descriptor bundle (.json/.yaml); repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("graph command needs exactly one topology path")
	}
	graphPath := flags.Args()[0]
	if *lockPath == "" {
		*lockPath = filepath.Join(filepath.Dir(graphPath), "openrealtime.lock")
	}
	if update && *output != "" {
		return errors.New("graph update writes only the resolution lock; use graph compile for Graph IR")
	}
	if !emitIR && !render && *output != "" {
		return errors.New("-out is valid only for graph compile or graph render")
	}
	file, err := loadGraphTopology(graphPath)
	if err != nil {
		return err
	}
	catalog, err := loadElementCatalog(descriptorPaths)
	if err != nil {
		return err
	}
	mode := resolve.Locked
	lock := resolve.Lock{}
	if update {
		mode = resolve.Update
	} else {
		body, readErr := os.ReadFile(*lockPath)
		if readErr != nil {
			return fmt.Errorf("read graph resolution lock %s: %w (run `openrealtime graph update` deliberately)", *lockPath, readErr)
		}
		lock, err = resolve.ParseLock(body)
		if err != nil {
			return fmt.Errorf("parse graph resolution lock %s: %w", *lockPath, err)
		}
	}
	compiled, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: mode, Loader: graphcompiler.FileLoader{},
	})
	if err != nil {
		return err
	}
	if *valuesPath != "" {
		document, loadErr := loadGraphValues(*valuesPath)
		if loadErr != nil {
			return loadErr
		}
		bound, bindErr := graphvalues.Bind(compiled.Graph, document)
		if bindErr != nil {
			return bindErr
		}
		compiled.Graph = bound.Graph
	}
	if update {
		payload, err := compiled.Lock.Marshal()
		if err != nil {
			return err
		}
		if err := atomicWrite(*lockPath, payload); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "updated %s\n%s\n", *lockPath, compiled.Graph.Fingerprint)
		return nil
	}
	profile, err := validationProfile(*profileName)
	if err != nil {
		return err
	}
	findings := graphvalidate.Check(compiled.Graph, profile)
	hasWarnings := false
	for _, finding := range findings {
		fmt.Fprintf(stderr, "%s %s", finding.Severity, finding.Code)
		if finding.Node != "" {
			fmt.Fprintf(stderr, " node=%s", finding.Node)
		}
		if finding.Edge != "" {
			fmt.Fprintf(stderr, " edge=%s", finding.Edge)
		}
		fmt.Fprintf(stderr, ": %s\n", finding.Message)
		hasWarnings = hasWarnings || finding.Severity == graphvalidate.Warning
	}
	if hasWarnings && *warningsAsErrors {
		return fmt.Errorf("graph failed %s validation because warnings-as-errors is enabled", profile.Name)
	}
	if emitIR {
		payload, err := compiled.Graph.Marshal()
		if err != nil {
			return err
		}
		if err := writeOrPrint(*output, payload, stdout); err != nil {
			return err
		}
	} else if render {
		var rendered string
		switch strings.ToLower(*renderFormat) {
		case "mermaid", "mmd":
			rendered, err = inspect.Mermaid(compiled.Graph)
		case "dot":
			rendered, err = inspect.DOT(compiled.Graph)
		default:
			return fmt.Errorf("graph render format must be mermaid or dot, got %q", *renderFormat)
		}
		if err != nil {
			return err
		}
		if err := writeOrPrint(*output, []byte(rendered), stdout); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "%s\n", compiled.Graph.Fingerprint)
	}
	return nil
}

func loadGraphValues(path string) (graphvalues.Document, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return graphvalues.Document{}, fmt.Errorf("read graph values %s: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return graphvalues.ParseYAML(path, body)
	case ".json":
		return graphvalues.ParseJSON(path, body)
	default:
		return graphvalues.Document{}, fmt.Errorf("graph values %s must use .yaml, .yml, or .json", path)
	}
}

func loadGraphTopology(path string) (syntax.File, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return syntax.File{}, fmt.Errorf("read graph %s: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ortg":
		return syntax.Parse(path, body)
	case ".yaml", ".yml":
		return manifest.ParseYAML(path, body)
	case ".json":
		return manifest.ParseJSON(path, body)
	default:
		return syntax.File{}, fmt.Errorf("graph topology %s must use .ortg, .yaml, .yml, or .json", path)
	}
}

func loadElementCatalog(paths []string) (*resolve.Catalog, error) {
	catalog, err := elements.Catalog()
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read element descriptor bundle %s: %w", path, err)
		}
		bundle, err := codec.Parse(path, body)
		if err != nil {
			return nil, err
		}
		for _, descriptor := range bundle.Elements {
			if err := catalog.Register(descriptor); err != nil {
				return nil, fmt.Errorf("register descriptor from %s: %w", path, err)
			}
		}
	}
	return catalog, nil
}

func validationProfile(name string) (graphvalidate.Profile, error) {
	switch name {
	case "core":
		return graphvalidate.Core, nil
	case "realtime-agent":
		return graphvalidate.RealtimeAgent, nil
	case "conversational-voice":
		return graphvalidate.ConversationalVoice, nil
	case "computer-use":
		return graphvalidate.ComputerUse, nil
	default:
		return graphvalidate.Profile{}, fmt.Errorf("unknown graph validation profile %q", name)
	}
}

func writeOrPrint(path string, payload []byte, stdout io.Writer) error {
	if path == "" || path == "-" {
		_, err := stdout.Write(payload)
		return err
	}
	return atomicWrite(path, payload)
}

func atomicWrite(path string, payload []byte) (resultErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if resultErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set temporary mode for %s: %w", path, err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
