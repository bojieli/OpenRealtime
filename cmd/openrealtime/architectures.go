package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	projectarch "github.com/bojieli/OpenRealtime/architecture"
)

// runArchitectures exposes the same catalog the server resolves. It is a
// project surface, not a generator: listing, showing, and validating all use
// the production parser and immutable fingerprints.
func runArchitectures(arguments []string, output io.Writer) error {
	command := "list"
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		command, arguments = strings.ToLower(strings.TrimSpace(arguments[0])), arguments[1:]
	}
	flags := flag.NewFlagSet("openrealtime architectures "+command, flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "external architecture catalog; empty uses the repository-owned catalog")
	jsonOutput := flags.Bool("json", false, "render machine-readable JSON")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	catalog, err := loadArchitectureCatalog(*catalogPath)
	if err != nil {
		return err
	}
	switch command {
	case "list":
		if flags.NArg() != 0 {
			return errors.New("architectures list accepts flags only")
		}
		if *jsonOutput {
			return writeArchitectureJSON(output, catalog)
		}
		fmt.Fprintf(output, "catalog %s\n", catalog.Fingerprint())
		for _, definition := range catalog.Sorted() {
			topology, _ := definition.Topology()
			fmt.Fprintf(output, "%-31s %-13s %-11s %s\n",
				definition.Ref(), definition.Stage, topology, definition.Summary)
		}
		return nil
	case "show":
		if flags.NArg() != 1 {
			return errors.New("architectures show requires one exact id@revision")
		}
		definition, err := catalog.Resolve(flags.Arg(0))
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeArchitectureJSON(output, definition)
		}
		topology, _ := definition.Topology()
		fmt.Fprintf(output, "%s  %s  %s\n", definition.Ref(), definition.Stage, topology)
		fmt.Fprintf(output, "fingerprint : %s\n", definition.Fingerprint())
		fmt.Fprintf(output, "summary     : %s\n", definition.Summary)
		fmt.Fprintf(output, "change      : %s\n", definition.Change)
		fmt.Fprintf(output, "interaction : %s, %s, %s/%s\n",
			definition.Interaction.Mode, definition.Interaction.Evidence,
			definition.Interaction.Transport, definition.Interaction.Handoff)
		fmt.Fprintf(output, "requires    : %s\n", strings.Join(definition.Requires.Names(), ", "))
		if definition.Interaction.EvidenceCapabilities == nil {
			fmt.Fprintln(output, "evidence    : legacy coarse label (not valid for new controlled experiments)")
		} else {
			fmt.Fprintf(output, "evidence    : %s\n",
				strings.Join(definition.Interaction.EvidenceCapabilities.Names(), ", "))
		}
		if definition.Interaction.Control == nil {
			fmt.Fprintln(output, "controllers : legacy unattested selection (not valid for new controlled experiments)")
		} else {
			fmt.Fprintf(output, "controllers : %s; arbitration=%s\n",
				strings.Join(definition.Interaction.Control.Selectors.Names(), ", "),
				definition.Interaction.Control.Arbitration)
		}
		if len(definition.DerivedFrom) > 0 {
			parents := make([]string, 0, len(definition.DerivedFrom))
			for _, parent := range definition.DerivedFrom {
				parents = append(parents, parent.String())
			}
			fmt.Fprintf(output, "derived from: %s\n", strings.Join(parents, ", "))
		}
		return nil
	case "validate":
		if flags.NArg() != 0 {
			return errors.New("architectures validate accepts flags only")
		}
		fmt.Fprintf(output, "ok  %d immutable revisions  %s\n",
			len(catalog.Architectures), catalog.Fingerprint())
		return nil
	default:
		return fmt.Errorf("architecture command must be list, show, or validate, got %q", command)
	}
}

func loadArchitectureCatalog(path string) (projectarch.Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return projectarch.Default(), nil
	}
	return projectarch.Read(path)
}

func writeArchitectureJSON(output io.Writer, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(payload))
	return err
}
