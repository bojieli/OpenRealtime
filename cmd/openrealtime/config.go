package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// loadConfig applies a YAML file to a flag set before the command line is
// parsed, so anything given on the line still wins.
//
// It exists because this binary has ninety-seven flags. A command line that
// long is not composable: a deployment cannot keep half of it in version
// control and vary the other half, cannot comment a choice, and cannot say why
// the reasoning budget is what it is. Every one of those flags is a decision
// somebody has to be able to record.
//
// Nesting maps onto the flag names rather than onto a second vocabulary:
//
//	fast:
//	  provider: google-openai
//	  model: gemini-3.5-flash
//	  effort: low
//
// sets fast-provider, fast-model and fast-effort. There is no schema to keep
// in step with the flags, which is the point - a flag added tomorrow is
// configurable today, and a key that matches no flag is an error rather than a
// silent typo, because a setting that does nothing is worse than one that
// refuses.
func loadConfig(flags *flag.FlagSet, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("parsing config %s: %w", path, err)
	}
	settings := map[string]string{}
	if err := flatten("", document, settings); err != nil {
		return fmt.Errorf("in config %s: %w", path, err)
	}
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	// Sorted so a malformed file reports the same first error every time.
	sort.Strings(names)
	for _, name := range names {
		if flags.Lookup(name) == nil {
			return fmt.Errorf("in config %s: %q is not a setting", path, name)
		}
		if err := flags.Set(name, settings[name]); err != nil {
			return fmt.Errorf("in config %s: %s: %w", path, name, err)
		}
	}
	return nil
}

// flatten turns nested keys into the dashed names the flags already use.
func flatten(prefix string, node map[string]any, into map[string]string) error {
	for key, value := range node {
		name := key
		if prefix != "" {
			name = prefix + "-" + key
		}
		switch typed := value.(type) {
		case map[string]any:
			if err := flatten(name, typed, into); err != nil {
				return err
			}
		case nil:
			// An empty value is how a file says "leave this alone", which is
			// different from setting it to the empty string.
		case bool:
			into[name] = strconv.FormatBool(typed)
		case int:
			into[name] = strconv.Itoa(typed)
		case float64:
			into[name] = strconv.FormatFloat(typed, 'g', -1, 64)
		case string:
			into[name] = typed
		default:
			return fmt.Errorf("%s: %T is not a setting value", name, value)
		}
	}
	return nil
}

// configPath reads -config out of the arguments before the flag set is parsed,
// because the file has to be applied first for the command line to override it.
func configPath(arguments []string) (string, error) {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-config" || argument == "--config":
			if index+1 >= len(arguments) {
				return "", fmt.Errorf("-config needs a path")
			}
			return arguments[index+1], nil
		case strings.HasPrefix(argument, "-config="):
			return strings.TrimPrefix(argument, "-config="), nil
		case strings.HasPrefix(argument, "--config="):
			return strings.TrimPrefix(argument, "--config="), nil
		}
	}
	return "", nil
}
