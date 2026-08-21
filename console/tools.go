package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Tool is one thing the console can do on this machine.
//
// The schema travels to the browser, which declares it to the session, and the
// same declaration carries the confirmation requirement - so what the model is
// told about a tool and what this process will actually do are one statement
// rather than two that can disagree.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Confirm is the requirement declared to the session: never, policy, or
	// always. It is also enforced here, because the browser is a user
	// interface and this process is what touches the disk.
	Confirm string `json:"confirm"`
	// Mutating marks a tool that can change something outside this process.
	Mutating bool `json:"mutating"`

	run func(context.Context, *Host, json.RawMessage) (string, error)
}

// Host executes tools inside one directory.
//
// The root is the whole of the blast radius and it is checked after symlinks
// are resolved, because a path that looks contained and resolves elsewhere is
// exactly the case a prefix test misses.
type Host struct {
	root           string
	tools          []Tool
	commandTimeout time.Duration
	maxOutputBytes int
}

// HostConfig configures the tool host.
type HostConfig struct {
	// Root bounds every path. Required.
	Root string
	// Enabled selects tools by name. Empty selects the read-only set, which
	// is the right default for something a model drives by voice.
	Enabled []string
	// CommandTimeout bounds one command.
	CommandTimeout time.Duration
	// MaxOutputBytes bounds what one tool returns, because a model reading a
	// hundred megabytes of build log is not a useful outcome for anyone.
	MaxOutputBytes int
}

// NewHost validates the configuration and selects the tool set.
func NewHost(config HostConfig) (*Host, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("a tool host requires a root directory")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve the root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the root %q is not a directory", root)
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 2 * time.Minute
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = 64 << 10
	}
	host := &Host{
		root: root, commandTimeout: config.CommandTimeout,
		maxOutputBytes: config.MaxOutputBytes,
	}
	selected, err := selectTools(config.Enabled)
	if err != nil {
		return nil, err
	}
	host.tools = selected
	return host, nil
}

// Root is the directory every path is resolved against.
func (host *Host) Root() string { return host.root }

// Tools is the declared set, in a stable order.
func (host *Host) Tools() []Tool { return host.tools }

// Lookup finds a declared tool.
func (host *Host) Lookup(name string) (Tool, bool) {
	for _, tool := range host.tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// Run executes one declared tool.
//
// Confirmation is not checked here: it is the caller's, because the decision
// is a person's and this function is what happens after they made it.
func (host *Host) Run(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	tool, declared := host.Lookup(name)
	if !declared {
		return "", fmt.Errorf("no tool named %q is declared", name)
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	output, err := tool.run(ctx, host, arguments)
	if err != nil {
		return "", err
	}
	return host.truncate(output), nil
}

func (host *Host) truncate(output string) string {
	if len(output) <= host.maxOutputBytes {
		return output
	}
	return output[:host.maxOutputBytes] + fmt.Sprintf(
		"\n\n[truncated at %d bytes]", host.maxOutputBytes)
}

// resolve turns a client-supplied path into an absolute one inside the root.
//
// Symlinks are resolved before the containment test, and a path that does not
// exist yet is tested through its nearest existing parent - otherwise creating
// a file would be unbounded in exactly the way opening one is not.
func (host *Host) resolve(path string) (string, error) {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" {
		return host.root, nil
	}
	if !filepath.IsAbs(cleaned) {
		cleaned = filepath.Join(host.root, cleaned)
	}
	cleaned = filepath.Clean(cleaned)

	probe := cleaned
	var trailing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			candidate := filepath.Join(append([]string{resolved}, trailing...)...)
			if !contains(host.root, candidate) {
				return "", fmt.Errorf("path %q is outside the root", path)
			}
			return candidate, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("path %q cannot be resolved", path)
		}
		trailing = append([]string{filepath.Base(probe)}, trailing...)
		probe = parent
	}
}

func contains(root, candidate string) bool {
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, root+string(os.PathSeparator))
}

func (host *Host) display(path string) string {
	relative, err := filepath.Rel(host.root, path)
	if err != nil {
		return path
	}
	if relative == "." {
		return "."
	}
	return relative
}

// --- the tool set -----------------------------------------------------------

func selectTools(enabled []string) ([]Tool, error) {
	all := allTools()
	if len(enabled) == 0 {
		enabled = []string{"read_file", "list_directory", "search_files"}
	}
	var selected []Tool
	for _, name := range enabled {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if name == "all" {
			return all, nil
		}
		found := false
		for _, tool := range all {
			if tool.Name == name {
				selected = append(selected, tool)
				found = true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(all))
			for _, tool := range all {
				names = append(names, tool.Name)
			}
			return nil, fmt.Errorf("no tool named %q; available: %s", name, strings.Join(names, ", "))
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no tools selected")
	}
	return selected, nil
}

func allTools() []Tool {
	return []Tool{
		{
			Name: "read_file",
			Description: "Read a UTF-8 text file from the developer's working directory. " +
				"Paths are relative to that directory.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the working directory."}
				},
				"required": ["path"],
				"additionalProperties": false
			}`),
			Confirm: "never",
			run:     readFile,
		},
		{
			Name:        "list_directory",
			Description: "List the entries of a directory in the developer's working directory.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Directory path; empty means the working directory itself."}
				},
				"additionalProperties": false
			}`),
			Confirm: "never",
			run:     listDirectory,
		},
		{
			Name: "search_files",
			Description: "Search file contents for a regular expression and return matching lines " +
				"with their file and line number.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Go regular expression."},
					"path": {"type": "string", "description": "Directory to search; empty means the whole working directory."},
					"max_results": {"type": "integer", "description": "Stop after this many matches. Default 50."}
				},
				"required": ["pattern"],
				"additionalProperties": false
			}`),
			Confirm: "never",
			run:     searchFiles,
		},
		{
			Name:        "write_file",
			Description: "Write a UTF-8 text file, creating or replacing it.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string"},
					"content": {"type": "string"}
				},
				"required": ["path", "content"],
				"additionalProperties": false
			}`),
			Confirm:  "always",
			Mutating: true,
			run:      writeFile,
		},
		{
			Name: "run_command",
			Description: "Run a shell command in the developer's working directory and return its " +
				"combined output. Long-running commands should be started in the background.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "Shell command line."}
				},
				"required": ["command"],
				"additionalProperties": false
			}`),
			Confirm:  "always",
			Mutating: true,
			run:      runCommand,
		},
	}
}

// decode reads a tool's arguments, accepting both shapes they arrive in.
//
// The Realtime protocol carries function call arguments as a JSON-encoded
// string rather than as an object - response.function_call_arguments.done
// gives a client `"{\"path\":\"notes.txt\"}"`, not `{"path":"notes.txt"}`.
// A client that forwards them verbatim is doing the most obvious thing, so
// this accepts that as well as an already-parsed object rather than making
// every caller remember to unwrap.
func decode(arguments json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return fmt.Errorf("arguments are not valid for this tool: %w", err)
		}
		trimmed = []byte(inner)
	}
	if len(bytes.TrimSpace(trimmed)) == 0 {
		trimmed = []byte("{}")
	}
	if err := json.Unmarshal(trimmed, into); err != nil {
		return fmt.Errorf("arguments are not valid for this tool: %w", err)
	}
	return nil
}

func readFile(_ context.Context, host *Host, arguments json.RawMessage) (string, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := decode(arguments, &input); err != nil {
		return "", err
	}
	path, err := host.resolve(input.Path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", host.display(path), err)
	}
	return string(content), nil
}

func listDirectory(_ context.Context, host *Host, arguments json.RawMessage) (string, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := decode(arguments, &input); err != nil {
		return "", err
	}
	path, err := host.resolve(input.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", host.display(path), err)
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s is empty", host.display(path)), nil
	}
	return strings.Join(lines, "\n"), nil
}

func searchFiles(ctx context.Context, host *Host, arguments json.RawMessage) (string, error) {
	var input struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results"`
	}
	if err := decode(arguments, &input); err != nil {
		return "", err
	}
	expression, err := regexp.Compile(input.Pattern)
	if err != nil {
		return "", fmt.Errorf("pattern is not a valid regular expression: %w", err)
	}
	root, err := host.resolve(input.Path)
	if err != nil {
		return "", err
	}
	if input.MaxResults <= 0 {
		input.MaxResults = 50
	}

	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			// Directories nobody means when they say "search the project",
			// and the ones large enough to make the answer arrive too late to
			// be part of a conversation.
			switch entry.Name() {
			case ".git", "node_modules", "vendor", ".runtime", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= input.MaxResults {
			return filepath.SkipAll
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 2<<20 || !info.Mode().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil || !isText(content) {
			return nil
		}
		for number, line := range strings.Split(string(content), "\n") {
			if len(matches) >= input.MaxResults {
				break
			}
			if expression.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s",
					host.display(path), number+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q", input.Pattern), nil
	}
	sort.Strings(matches)
	return strings.Join(matches, "\n"), nil
}

// isText rejects binary files by looking for a NUL in the first block, which
// is the same cheap test grep uses and wrong in the same rare ways.
func isText(content []byte) bool {
	limit := min(len(content), 8000)
	for _, b := range content[:limit] {
		if b == 0 {
			return false
		}
	}
	return true
}

func writeFile(_ context.Context, host *Host, arguments json.RawMessage) (string, error) {
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(arguments, &input); err != nil {
		return "", err
	}
	if strings.TrimSpace(input.Path) == "" {
		return "", errors.New("write_file requires a path")
	}
	path, err := host.resolve(input.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create the parent directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(input.Content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", host.display(path), err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(input.Content), host.display(path)), nil
}

func runCommand(ctx context.Context, host *Host, arguments json.RawMessage) (string, error) {
	var input struct {
		Command string `json:"command"`
	}
	if err := decode(arguments, &input); err != nil {
		return "", err
	}
	if strings.TrimSpace(input.Command) == "" {
		return "", errors.New("run_command requires a command")
	}
	bounded, cancel := context.WithTimeout(ctx, host.commandTimeout)
	defer cancel()
	command := exec.CommandContext(bounded, "sh", "-c", input.Command)
	command.Dir = host.root
	output, err := command.CombinedOutput()
	text := strings.TrimRight(string(output), "\n")
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("the command did not finish within %s; output so far:\n%s",
			host.commandTimeout, text)
	}
	if err != nil {
		// A non-zero exit is a result rather than a failure of the tool: the
		// model asked what happens when this runs, and this is what happened.
		return fmt.Sprintf("exit status: %v\n%s", err, text), nil
	}
	if text == "" {
		return "the command produced no output", nil
	}
	return text, nil
}
