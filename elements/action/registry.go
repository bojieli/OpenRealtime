package action

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type declaredTool struct {
	spec               legacyaction.ToolSpec
	digest             string
	dispatcherIdentity string
}

type toolSet struct {
	reference string
	digest    string
	tools     map[string]declaredTool
}

// ToolRegistries is deployment-owned immutable action surface selection. A
// registration snapshots the existing action.ToolSpec contract; later
// mutation of a legacy Registry cannot silently change a mounted graph.
type ToolRegistries struct {
	mu      sync.RWMutex
	entries map[string]toolSet
}

func NewToolRegistries() *ToolRegistries {
	return &ToolRegistries{entries: make(map[string]toolSet)}
}

func (registries *ToolRegistries) RegisterLegacy(reference string, registry *legacyaction.Registry) error {
	if registry == nil {
		return errors.New("register tool registry: nil legacy registry")
	}
	return registries.Register(reference, registry.Specs())
}

func (registries *ToolRegistries) Register(reference string, specs []legacyaction.ToolSpec) error {
	if registries == nil {
		return errors.New("register tool registry: nil registries")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register tool registry: empty reference")
	}
	validated := legacyaction.NewRegistry()
	for _, spec := range specs {
		if err := validated.Declare(spec); err != nil {
			return fmt.Errorf("register tool registry %q: %w", reference, err)
		}
	}
	normalized := validated.Specs()
	tools := make(map[string]declaredTool, len(normalized))
	type digestTool struct {
		Name        string               `json:"name"`
		Description string               `json:"description"`
		Parameters  json.RawMessage      `json:"parameters"`
		Confirm     legacyaction.Confirm `json:"confirm"`
		Background  bool                 `json:"background"`
		Target      string               `json:"target"`
		Dispatcher  string               `json:"dispatcher"`
	}
	digestTools := make([]digestTool, 0, len(normalized))
	for _, spec := range normalized {
		identity := ""
		if spec.Dispatcher != nil && !reflectedNil(spec.Dispatcher) {
			identity = strings.TrimSpace(spec.Dispatcher.Name())
			if identity == "" {
				return fmt.Errorf("register tool registry %q: tool %q dispatcher has empty identity", reference, spec.Name)
			}
		}
		copy := spec
		copy.Parameters = slices.Clone(spec.Parameters)
		declarationDigest, err := digestJSON(digestTool{
			Name: copy.Name, Description: copy.Description, Parameters: copy.Parameters,
			Confirm: copy.Confirm, Background: copy.Background, Target: copy.Target, Dispatcher: identity,
		})
		if err != nil {
			return fmt.Errorf("register tool registry %q: digest tool %q: %w", reference, spec.Name, err)
		}
		tools[copy.Name] = declaredTool{spec: copy, digest: declarationDigest, dispatcherIdentity: identity}
		digestTools = append(digestTools, digestTool{
			Name: copy.Name, Description: copy.Description, Parameters: copy.Parameters,
			Confirm: copy.Confirm, Background: copy.Background, Target: copy.Target, Dispatcher: identity,
		})
	}
	// Legacy Registry preserves declaration order, but sort by name for an
	// identity that is stable across equivalent deployment construction.
	sort.Slice(digestTools, func(i, j int) bool { return digestTools[i].Name < digestTools[j].Name })
	digest, err := digestJSON(digestTools)
	if err != nil {
		return fmt.Errorf("register tool registry %q: %w", reference, err)
	}
	entry := toolSet{reference: reference, digest: digest, tools: tools}
	registries.mu.Lock()
	defer registries.mu.Unlock()
	if registries.entries == nil {
		registries.entries = make(map[string]toolSet)
	}
	if _, duplicate := registries.entries[reference]; duplicate {
		return fmt.Errorf("tool registry %q is already registered", reference)
	}
	registries.entries[reference] = entry
	return nil
}

func (registries *ToolRegistries) resolve(reference string) (toolSet, error) {
	if registries == nil {
		return toolSet{}, errors.New("tool registries service is nil")
	}
	reference = strings.TrimSpace(reference)
	registries.mu.RLock()
	entry, found := registries.entries[reference]
	registries.mu.RUnlock()
	if !found {
		return toolSet{}, fmt.Errorf("tool registry %q is not registered", reference)
	}
	return entry, nil
}

func (set toolSet) lookup(name string) (declaredTool, bool, error) {
	tool, found := set.tools[name]
	if !found {
		return declaredTool{}, false, nil
	}
	if tool.spec.Dispatcher != nil && !reflectedNil(tool.spec.Dispatcher) {
		actual := strings.TrimSpace(tool.spec.Dispatcher.Name())
		if actual != tool.dispatcherIdentity {
			return declaredTool{}, false, fmt.Errorf("tool %q dispatcher identity drifted: registered %q, live %q",
				name, tool.dispatcherIdentity, actual)
		}
	}
	return tool, true, nil
}

// ConfirmationProvider extends the existing confirmer behavior with one
// stable live identity. io.Closer is honored when implemented.
type ConfirmationProvider interface {
	legacyaction.Confirmer
	Name() string
}

type ConfirmationProviderFunc struct {
	Identity string
	Function func(context.Context, legacyaction.ConfirmationRequest) (bool, error)
}

func (provider ConfirmationProviderFunc) Name() string { return provider.Identity }
func (provider ConfirmationProviderFunc) Confirm(ctx context.Context, request legacyaction.ConfirmationRequest) (bool, error) {
	if provider.Function == nil {
		return false, errors.New("confirmation provider has no function")
	}
	return provider.Function(ctx, request)
}

type ConfirmationProviderFactory func() (ConfirmationProvider, error)

type confirmationRegistration struct {
	identity string
	factory  ConfirmationProviderFactory
	secret   [sha256.Size]byte
}

type ConfirmationProviders struct {
	mu      sync.RWMutex
	entries map[string]confirmationRegistration
}

func NewConfirmationProviders() *ConfirmationProviders {
	return &ConfirmationProviders{entries: make(map[string]confirmationRegistration)}
}

func (providers *ConfirmationProviders) Register(
	reference, identity string, factory ConfirmationProviderFactory,
) error {
	if providers == nil {
		return errors.New("register confirmation provider: nil registry")
	}
	reference, identity = strings.TrimSpace(reference), strings.TrimSpace(identity)
	if reference == "" || identity == "" || factory == nil {
		return errors.New("confirmation provider requires reference, identity, and factory")
	}
	var secret [sha256.Size]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("create confirmation capability key: %w", err)
	}
	providers.mu.Lock()
	defer providers.mu.Unlock()
	if providers.entries == nil {
		providers.entries = make(map[string]confirmationRegistration)
	}
	if _, duplicate := providers.entries[reference]; duplicate {
		return fmt.Errorf("confirmation provider %q is already registered", reference)
	}
	providers.entries[reference] = confirmationRegistration{identity: identity, factory: factory, secret: secret}
	return nil
}

func (entry confirmationRegistration) sign(reference string, declared DeclaredAction) (string, error) {
	encoded, err := json.Marshal(struct {
		Declared  DeclaredAction `json:"declared"`
		Reference string         `json:"reference"`
		Identity  string         `json:"identity"`
	}{Declared: declared, Reference: reference, Identity: entry.identity})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, entry.secret[:])
	_, _ = mac.Write(encoded)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (entry confirmationRegistration) verify(confirmed ConfirmedAction) bool {
	want, err := entry.sign(confirmed.ProviderReference, confirmed.Declared)
	return err == nil && confirmed.ProviderIdentity == entry.identity &&
		hmac.Equal([]byte(want), []byte(confirmed.ConfirmationCapability))
}

func (providers *ConfirmationProviders) resolve(reference string) (confirmationRegistration, error) {
	if providers == nil {
		return confirmationRegistration{}, errors.New("confirmation provider registry is nil")
	}
	providers.mu.RLock()
	entry, found := providers.entries[strings.TrimSpace(reference)]
	providers.mu.RUnlock()
	if !found {
		return confirmationRegistration{}, fmt.Errorf("confirmation provider %q is not registered", reference)
	}
	return entry, nil
}

func createConfirmationProvider(reference string, entry confirmationRegistration) (ConfirmationProvider, error) {
	provider, err := entry.factory()
	if err != nil {
		return nil, fmt.Errorf("create confirmation provider %q: %w", reference, err)
	}
	if provider == nil || reflectedNil(provider) {
		return nil, fmt.Errorf("confirmation provider %q factory returned nil", reference)
	}
	if actual := strings.TrimSpace(provider.Name()); actual != entry.identity {
		_ = closeIfPossible(provider)
		return nil, fmt.Errorf("confirmation provider %q identity drifted: registered %q, live %q",
			reference, entry.identity, actual)
	}
	return provider, nil
}

type targetEntry struct {
	reference string
	target    computeruse.Target
	digest    string
}

type TargetRegistries struct {
	mu      sync.RWMutex
	entries map[string]targetEntry
}

func NewTargetRegistries() *TargetRegistries {
	return &TargetRegistries{entries: make(map[string]targetEntry)}
}

func (registries *TargetRegistries) Register(reference string, target computeruse.Target) error {
	if registries == nil {
		return errors.New("register target: nil registries")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register target: empty reference")
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("register target %q: %w", reference, err)
	}
	target.Sources = slices.Clone(target.Sources)
	digest, err := digestJSON(target)
	if err != nil {
		return err
	}
	registries.mu.Lock()
	defer registries.mu.Unlock()
	if registries.entries == nil {
		registries.entries = make(map[string]targetEntry)
	}
	if _, duplicate := registries.entries[reference]; duplicate {
		return fmt.Errorf("target %q is already registered", reference)
	}
	registries.entries[reference] = targetEntry{reference: reference, target: target, digest: digest}
	return nil
}

func (registries *TargetRegistries) resolve(reference string) (targetEntry, error) {
	if registries == nil {
		return targetEntry{}, errors.New("target registries service is nil")
	}
	registries.mu.RLock()
	entry, found := registries.entries[strings.TrimSpace(reference)]
	registries.mu.RUnlock()
	if !found {
		return targetEntry{}, fmt.Errorf("target %q is not registered", reference)
	}
	entry.target.Sources = slices.Clone(entry.target.Sources)
	return entry, nil
}

type ledgerEntry struct {
	reference string
	identity  string
	ledger    *legacyaction.Ledger
	secret    [sha256.Size]byte
}

type LedgerRegistries struct {
	mu      sync.RWMutex
	entries map[string]ledgerEntry
}

func NewLedgerRegistries() *LedgerRegistries {
	return &LedgerRegistries{entries: make(map[string]ledgerEntry)}
}

func (registries *LedgerRegistries) Register(reference string, ledger *legacyaction.Ledger) error {
	if registries == nil {
		return errors.New("register ledger: nil registries")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" || ledger == nil {
		return errors.New("register ledger requires reference and non-nil ledger")
	}
	var secret [sha256.Size]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("create ledger capability key: %w", err)
	}
	public := sha256.Sum256(secret[:])
	entry := ledgerEntry{
		reference: reference, identity: "sha256:" + hex.EncodeToString(public[:]), ledger: ledger, secret: secret,
	}
	registries.mu.Lock()
	defer registries.mu.Unlock()
	if registries.entries == nil {
		registries.entries = make(map[string]ledgerEntry)
	}
	if _, duplicate := registries.entries[reference]; duplicate {
		return fmt.Errorf("ledger %q is already registered", reference)
	}
	registries.entries[reference] = entry
	return nil
}

func (registries *LedgerRegistries) resolve(reference string) (ledgerEntry, error) {
	if registries == nil {
		return ledgerEntry{}, errors.New("ledger registries service is nil")
	}
	registries.mu.RLock()
	entry, found := registries.entries[strings.TrimSpace(reference)]
	registries.mu.RUnlock()
	if !found {
		return ledgerEntry{}, fmt.Errorf("ledger %q is not registered", reference)
	}
	if entry.ledger == nil {
		return ledgerEntry{}, fmt.Errorf("ledger %q resolved nil", reference)
	}
	return entry, nil
}

func (entry ledgerEntry) sign(action CanonicalAction, commitmentID string) (string, error) {
	encoded, err := capabilityPayload(action, commitmentID, entry.reference, entry.identity)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, entry.secret[:])
	_, _ = mac.Write(encoded)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (entry ledgerEntry) verify(executable ExecutableAction) bool {
	want, err := entry.sign(executable.Canonical, executable.CommitmentID)
	return err == nil && hmac.Equal([]byte(want), []byte(executable.Capability)) &&
		executable.LedgerReference == entry.reference && executable.LedgerIdentity == entry.identity
}

func (entry ledgerEntry) signResult(result ExecutionResult) (string, error) {
	encoded, err := resultCapabilityPayload(result)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, entry.secret[:])
	_, _ = mac.Write(encoded)
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (entry ledgerEntry) verifyResult(result ExecutionResult) bool {
	want, err := entry.signResult(result)
	return err == nil && hmac.Equal([]byte(want), []byte(result.ResultCapability)) &&
		entry.verify(result.Executable)
}

func resultCapabilityPayload(result ExecutionResult) ([]byte, error) {
	return json.Marshal(struct {
		Executable   ExecutableAction      `json:"executable"`
		CallID       string                `json:"call_id"`
		Name         string                `json:"name"`
		CommitmentID string                `json:"commitment_id"`
		Result       trajectory.ToolResult `json:"result"`
		CrossedNS    uint64                `json:"crossed_ns"`
		FinishedNS   uint64                `json:"finished_ns"`
	}{
		Executable: result.Executable, CallID: result.CallID, Name: result.Name,
		CommitmentID: result.CommitmentID, Result: result.Result,
		CrossedNS: result.CrossedNS, FinishedNS: result.FinishedNS,
	})
}

func capabilityPayload(action CanonicalAction, commitmentID, ledgerReference, ledgerIdentity string) ([]byte, error) {
	return json.Marshal(struct {
		Canonical  CanonicalAction `json:"canonical"`
		Commitment string          `json:"commitment"`
		Ledger     string          `json:"ledger"`
		Identity   string          `json:"identity"`
	}{
		Canonical: action, Commitment: commitmentID, Ledger: ledgerReference, Identity: ledgerIdentity,
	})
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func reflectedNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type closer interface{ Close() error }

func closeIfPossible(value any) error {
	if value == nil || reflectedNil(value) {
		return nil
	}
	if closeable, ok := value.(closer); ok {
		return closeable.Close()
	}
	return nil
}
