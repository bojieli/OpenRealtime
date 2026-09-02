package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// errBoundaryGenerationRetired is private to the stable boundary wrappers. It
// means that the concrete operation was interrupted before it committed any
// queue mutation, so (and only so) the wrapper may retry on a later
// generation.
var errBoundaryGenerationRetired = errors.New("graph boundary generation retired before commit")

// routeEpoch separates leases admitted before retirement from leases admitted
// after a refused transition resumes the same concrete generation. Old leases
// retain their own accounting even after a new epoch becomes current.
type routeEpoch struct {
	retired chan struct{}

	active   int
	retiring bool
}

// routeAdmission is an acquire-versus-retire handshake. The mutex also acts
// as the commit token used immediately before queue mutation: retirement
// either closes admission first, or waits for the already-authorized mutation
// to finish. It is deliberately not an RWMutex or WaitGroup; both have unsafe
// admission/wait races for this use.
type routeAdmission struct {
	mu         sync.Mutex
	current    *routeEpoch
	open       bool
	active     int
	allDrained chan struct{}
}

func newRouteAdmission() *routeAdmission {
	drained := make(chan struct{})
	close(drained)
	return &routeAdmission{
		current:    &routeEpoch{retired: make(chan struct{})},
		open:       true,
		allDrained: drained,
	}
}

func (admission *routeAdmission) acquire() (*routeLease, bool) {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if !admission.open {
		return nil, false
	}
	epoch := admission.current
	if admission.active == 0 {
		admission.allDrained = make(chan struct{})
	}
	admission.active++
	epoch.active++
	return &routeLease{admission: admission, epoch: epoch}, true
}

// retire closes admission and wakes every operation admitted in the current
// epoch. The epoch is returned so a pre-publication refusal can resume without
// confusing it with a concurrently installed epoch.
func (admission *routeAdmission) retire(ctx context.Context) (*routeEpoch, error) {
	epoch, drained := admission.stop()
	if err := context.Cause(ctx); err != nil {
		return epoch, err
	}
	select {
	case <-drained:
		if err := context.Cause(ctx); err != nil {
			return epoch, err
		}
		return epoch, nil
	case <-ctx.Done():
		return epoch, context.Cause(ctx)
	}
}

func (admission *routeAdmission) stop() (*routeEpoch, <-chan struct{}) {
	admission.mu.Lock()
	epoch := admission.current
	if admission.open {
		admission.open = false
		epoch.retiring = true
		close(epoch.retired)
	}
	drained := admission.allDrained
	admission.mu.Unlock()
	return epoch, drained
}

// resume opens a fresh epoch on the same concrete generation. It is safe even
// if an old lease is still returning from a committed observer callback: an
// old lease can no longer pass beginCommit because it is not current.
func (admission *routeAdmission) resume(retired *routeEpoch) bool {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.open || admission.current != retired || !retired.retiring {
		return false
	}
	admission.current = &routeEpoch{retired: make(chan struct{})}
	admission.open = true
	return true
}

func (admission *routeAdmission) activeLeases() int {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return admission.active
}

// routeLease is held for one complete concrete port/lane operation. The
// admission mutex is acquired only around the queue commit point, not while a
// lossless operation is blocked on capacity.
type routeLease struct {
	admission *routeAdmission
	epoch     *routeEpoch
	released  bool
}

// beginCommit returns with the admission mutex held on success. Holding that
// token until endCommit linearizes retirement against the queue mutation.
// Methods are nil-safe so ordinary non-routed channels retain their old path.
func (lease *routeLease) beginCommit() bool {
	if lease == nil {
		return true
	}
	admission := lease.admission
	admission.mu.Lock()
	if lease.released || !admission.open || admission.current != lease.epoch || lease.epoch.retiring {
		admission.mu.Unlock()
		return false
	}
	return true
}

func (lease *routeLease) endCommit() {
	if lease != nil {
		lease.admission.mu.Unlock()
	}
}

func (lease *routeLease) retiredSignal() <-chan struct{} {
	if lease == nil {
		return nil
	}
	return lease.epoch.retired
}

func (lease *routeLease) retired() bool {
	if lease == nil {
		return false
	}
	select {
	case <-lease.epoch.retired:
		return true
	default:
		return false
	}
}

func (lease *routeLease) release() {
	if lease == nil {
		return
	}
	admission := lease.admission
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if lease.released {
		return
	}
	lease.released = true
	lease.epoch.active--
	admission.active--
	if lease.epoch.active < 0 || admission.active < 0 {
		panic("graph boundary route lease accounting underflow")
	}
	if admission.active == 0 {
		close(admission.allDrained)
	}
}

type boundaryPortContract struct {
	name        string
	direction   ir.BoundaryDirection
	typ         element.Type
	cardinality element.Cardinality
	laneIDs     []string
}

func (contract boundaryPortContract) clone() boundaryPortContract {
	contract.typ = contract.typ.Clone()
	contract.laneIDs = append([]string(nil), contract.laneIDs...)
	return contract
}

type boundaryContractSet struct {
	ingress map[string]boundaryPortContract
	egress  map[string]boundaryPortContract
}

func (contracts boundaryContractSet) clone() boundaryContractSet {
	clone := boundaryContractSet{
		ingress: make(map[string]boundaryPortContract, len(contracts.ingress)),
		egress:  make(map[string]boundaryPortContract, len(contracts.egress)),
	}
	for name, contract := range contracts.ingress {
		clone.ingress[name] = contract.clone()
	}
	for name, contract := range contracts.egress {
		clone.egress[name] = contract.clone()
	}
	return clone
}

type boundaryOutputRoute struct {
	port      *outputPort
	laneOrder []string
	lanes     map[string]*sender
}

type boundaryInputRoute struct {
	port      *inputPort
	laneOrder []string
	lanes     map[string]*receiver
}

// boundaryGeneration is immutable after construction. Both route tables are
// published together; the directional admissions are retired independently so
// predecessor egress can remain drainable after ingress has paused.
type boundaryGeneration struct {
	sequence uint64
	ingress  map[string]boundaryOutputRoute
	egress   map[string]boundaryInputRoute

	ingressAdmission *routeAdmission
	egressAdmission  *routeAdmission
}

func prepareBoundaryGeneration(
	ingress map[string]*outputPort, egress map[string]*inputPort,
) (*boundaryGeneration, boundaryContractSet, error) {
	generation := &boundaryGeneration{
		ingress:          make(map[string]boundaryOutputRoute, len(ingress)),
		egress:           make(map[string]boundaryInputRoute, len(egress)),
		ingressAdmission: newRouteAdmission(),
		egressAdmission:  newRouteAdmission(),
	}
	contracts := boundaryContractSet{
		ingress: make(map[string]boundaryPortContract, len(ingress)),
		egress:  make(map[string]boundaryPortContract, len(egress)),
	}
	for name, port := range ingress {
		route, contract, err := prepareBoundaryOutputRoute(name, port)
		if err != nil {
			return nil, boundaryContractSet{}, fmt.Errorf("prepare graph input boundary %q: %w", name, err)
		}
		generation.ingress[name] = route
		contracts.ingress[name] = contract
	}
	for name, port := range egress {
		route, contract, err := prepareBoundaryInputRoute(name, port)
		if err != nil {
			return nil, boundaryContractSet{}, fmt.Errorf("prepare graph output boundary %q: %w", name, err)
		}
		generation.egress[name] = route
		contracts.egress[name] = contract
	}
	return generation, contracts, nil
}

func prepareBoundaryOutputRoute(
	name string, port *outputPort,
) (boundaryOutputRoute, boundaryPortContract, error) {
	if name == "" {
		return boundaryOutputRoute{}, boundaryPortContract{}, errors.New("boundary name is empty")
	}
	if port == nil {
		return boundaryOutputRoute{}, boundaryPortContract{}, errors.New("boundary port is nil")
	}
	if port.name != name {
		return boundaryOutputRoute{}, boundaryPortContract{}, fmt.Errorf("port name is %q", port.name)
	}
	if err := port.typ.ValidateConcretePort(); err != nil {
		return boundaryOutputRoute{}, boundaryPortContract{}, fmt.Errorf("type: %w", err)
	}
	if len(port.queues) == 0 {
		return boundaryOutputRoute{}, boundaryPortContract{}, errors.New("boundary has no lane")
	}
	laneOrder := make([]string, len(port.queues))
	lanes := make(map[string]*sender, len(port.queues))
	for index, queue := range port.queues {
		if queue == nil {
			return boundaryOutputRoute{}, boundaryPortContract{}, fmt.Errorf("lane %d is nil", index)
		}
		if !queue.valueType.Equal(port.typ) {
			return boundaryOutputRoute{}, boundaryPortContract{}, fmt.Errorf(
				"lane %q type is %s, port requires %s", queue.id, queue.valueType.String(), port.typ.String(),
			)
		}
		if _, duplicate := lanes[queue.id]; duplicate {
			return boundaryOutputRoute{}, boundaryPortContract{}, fmt.Errorf("lane ID %q is duplicated", queue.id)
		}
		laneOrder[index] = queue.id
		lanes[queue.id] = &sender{queue: queue, observe: port.observe}
	}
	cardinality := element.Variadic
	if len(laneOrder) == 1 {
		cardinality = element.One
	}
	contract := boundaryPortContract{
		name: name, direction: ir.InputBoundary, typ: port.typ.Clone(),
		cardinality: cardinality, laneIDs: append([]string(nil), laneOrder...),
	}
	return boundaryOutputRoute{port: port, laneOrder: laneOrder, lanes: lanes}, contract, nil
}

func prepareBoundaryInputRoute(
	name string, port *inputPort,
) (boundaryInputRoute, boundaryPortContract, error) {
	if name == "" {
		return boundaryInputRoute{}, boundaryPortContract{}, errors.New("boundary name is empty")
	}
	if port == nil {
		return boundaryInputRoute{}, boundaryPortContract{}, errors.New("boundary port is nil")
	}
	if port.name != name {
		return boundaryInputRoute{}, boundaryPortContract{}, fmt.Errorf("port name is %q", port.name)
	}
	if err := port.typ.ValidateConcretePort(); err != nil {
		return boundaryInputRoute{}, boundaryPortContract{}, fmt.Errorf("type: %w", err)
	}
	if len(port.queues) == 0 {
		return boundaryInputRoute{}, boundaryPortContract{}, errors.New("boundary has no lane")
	}
	laneOrder := make([]string, len(port.queues))
	lanes := make(map[string]*receiver, len(port.queues))
	for index, queue := range port.queues {
		if queue == nil {
			return boundaryInputRoute{}, boundaryPortContract{}, fmt.Errorf("lane %d is nil", index)
		}
		if !queue.valueType.Equal(port.typ) {
			return boundaryInputRoute{}, boundaryPortContract{}, fmt.Errorf(
				"lane %q type is %s, port requires %s", queue.id, queue.valueType.String(), port.typ.String(),
			)
		}
		if _, duplicate := lanes[queue.id]; duplicate {
			return boundaryInputRoute{}, boundaryPortContract{}, fmt.Errorf("lane ID %q is duplicated", queue.id)
		}
		laneOrder[index] = queue.id
		lanes[queue.id] = &receiver{queue: queue, observe: port.observe}
	}
	cardinality := element.Variadic
	if len(laneOrder) == 1 {
		cardinality = element.One
	}
	contract := boundaryPortContract{
		name: name, direction: ir.OutputBoundary, typ: port.typ.Clone(),
		cardinality: cardinality, laneIDs: append([]string(nil), laneOrder...),
	}
	return boundaryInputRoute{port: port, laneOrder: laneOrder, lanes: lanes}, contract, nil
}

func validateBoundaryContracts(expected, candidate boundaryContractSet) error {
	if err := validateBoundaryDirection("input", expected.ingress, candidate.ingress); err != nil {
		return err
	}
	return validateBoundaryDirection("output", expected.egress, candidate.egress)
}

func validateBoundaryDirection(
	direction string,
	expected map[string]boundaryPortContract,
	candidate map[string]boundaryPortContract,
) error {
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := expected[name]
		got, found := candidate[name]
		if !found {
			return fmt.Errorf("candidate is missing graph %s boundary %q", direction, name)
		}
		switch {
		case got.name != want.name:
			return fmt.Errorf("candidate graph %s boundary %q has name %q", direction, name, got.name)
		case got.direction != want.direction:
			return fmt.Errorf("candidate graph %s boundary %q has direction %q, want %q",
				direction, name, got.direction, want.direction)
		case !got.typ.Equal(want.typ):
			return fmt.Errorf("candidate graph %s boundary %q has type %s, want %s",
				direction, name, got.typ.String(), want.typ.String())
		case got.cardinality != want.cardinality:
			return fmt.Errorf("candidate graph %s boundary %q has cardinality %q, want %q",
				direction, name, got.cardinality, want.cardinality)
		case !equalStrings(got.laneIDs, want.laneIDs):
			return fmt.Errorf("candidate graph %s boundary %q has lane IDs %v, want %v",
				direction, name, got.laneIDs, want.laneIDs)
		}
	}
	extra := make([]string, 0)
	for name := range candidate {
		if _, found := expected[name]; !found {
			extra = append(extra, name)
		}
	}
	if len(extra) != 0 {
		sort.Strings(extra)
		return fmt.Errorf("candidate has unexpected graph %s boundary %q", direction, extra[0])
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// boundaryRouter owns stable public handles for the lifetime of one Mounted
// facade. Concrete generations remain private and disposable.
type boundaryRouter struct {
	mu              sync.Mutex
	current         *boundaryGeneration
	contracts       boundaryContractSet
	changed         *condition
	closed          bool
	drainableEgress bool
	queueShutdown   atomic.Bool
	closedCh        chan struct{}

	transitionToken chan struct{}
	ingress         map[string]*stableOutputPort
	egress          map[string]*stableInputPort
}

func newBoundaryRouter(
	ingress map[string]*outputPort, egress map[string]*inputPort,
) (*boundaryRouter, error) {
	generation, contracts, err := prepareBoundaryGeneration(ingress, egress)
	if err != nil {
		return nil, err
	}
	generation.sequence = 1
	router := &boundaryRouter{
		current: generation, contracts: contracts.clone(), changed: newCondition(),
		closedCh: make(chan struct{}), transitionToken: make(chan struct{}, 1),
		ingress: make(map[string]*stableOutputPort, len(contracts.ingress)),
		egress:  make(map[string]*stableInputPort, len(contracts.egress)),
	}
	router.transitionToken <- struct{}{}
	for name, contract := range contracts.ingress {
		stable := &stableOutputPort{router: router, contract: contract.clone()}
		stable.lanes = make([]element.Sender, len(contract.laneIDs))
		for index, laneID := range contract.laneIDs {
			stable.lanes[index] = &stableSender{
				router: router, boundary: name, id: laneID, typ: contract.typ.Clone(),
			}
		}
		router.ingress[name] = stable
	}
	for name, contract := range contracts.egress {
		stable := &stableInputPort{router: router, contract: contract.clone()}
		stable.lanes = make([]element.Receiver, len(contract.laneIDs))
		for index, laneID := range contract.laneIDs {
			stable.lanes[index] = &stableReceiver{
				router: router, boundary: name, id: laneID, typ: contract.typ.Clone(),
			}
		}
		router.egress[name] = stable
	}
	return router, nil
}

func (router *boundaryRouter) acquire(
	ctx context.Context, direction ir.BoundaryDirection,
) (*boundaryGeneration, *routeLease, error) {
	for {
		router.mu.Lock()
		finalDrain := direction == ir.OutputBoundary && router.closed && router.drainableEgress
		if router.closed && !finalDrain {
			router.mu.Unlock()
			return nil, nil, ErrChannelClosed
		}
		generation := router.current
		admission := generation.egressAdmission
		if direction == ir.InputBoundary {
			admission = generation.ingressAdmission
		}
		if lease, admitted := admission.acquire(); admitted {
			router.mu.Unlock()
			return generation, lease, nil
		}
		wait := router.changed.current()
		var closed <-chan struct{}
		if !finalDrain {
			closed = router.closedCh
		}
		router.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, context.Cause(ctx)
		case <-closed:
			return nil, nil, ErrChannelClosed
		case <-wait:
		}
	}
}

func (router *boundaryRouter) input(name string) (*stableOutputPort, bool) {
	port, found := router.ingress[name]
	return port, found
}

func (router *boundaryRouter) output(name string) (*stableInputPort, bool) {
	port, found := router.egress[name]
	return port, found
}

// replace is a bounded routing transaction used by the future reconciliation
// layer. It intentionally does not assert that graph work is quiescent: the
// supplied drain callback must establish that stronger proof before cutover.
// This private primitive therefore is not, by itself, topology reconciliation.
func (router *boundaryRouter) replace(
	ctx context.Context,
	candidateIngress map[string]*outputPort,
	candidateEgress map[string]*inputPort,
	drain func(context.Context) error,
) error {
	if ctx == nil {
		return errors.New("replace graph boundary generation: nil context")
	}
	candidate, contracts, err := prepareBoundaryGeneration(candidateIngress, candidateEgress)
	if err != nil {
		return fmt.Errorf("replace graph boundary generation: %w", err)
	}
	if err := validateBoundaryContracts(router.contracts, contracts); err != nil {
		return fmt.Errorf("replace graph boundary generation: %w", err)
	}

	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-router.closedCh:
		return ErrChannelClosed
	case <-router.transitionToken:
	}
	defer func() { router.transitionToken <- struct{}{} }()

	router.mu.Lock()
	if router.closed {
		router.mu.Unlock()
		return ErrChannelClosed
	}
	predecessor := router.current
	candidate.sequence = predecessor.sequence + 1
	router.mu.Unlock()

	ingressEpoch, err := predecessor.ingressAdmission.retire(ctx)
	if err != nil {
		router.resume(predecessor, ingressEpoch, nil)
		return fmt.Errorf("pause graph boundary ingress: %w", err)
	}
	if drain != nil {
		if err := drain(ctx); err != nil {
			router.resume(predecessor, ingressEpoch, nil)
			return fmt.Errorf("drain graph boundary egress: %w", err)
		}
	}
	egressEpoch, err := predecessor.egressAdmission.retire(ctx)
	if err != nil {
		router.resume(predecessor, ingressEpoch, egressEpoch)
		return fmt.Errorf("pause graph boundary egress: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		router.resume(predecessor, ingressEpoch, egressEpoch)
		return err
	}

	router.mu.Lock()
	if router.closed {
		router.mu.Unlock()
		router.resume(predecessor, ingressEpoch, egressEpoch)
		return ErrChannelClosed
	}
	if router.current != predecessor {
		router.mu.Unlock()
		router.resume(predecessor, ingressEpoch, egressEpoch)
		return errors.New("graph boundary generation changed during serialized replacement")
	}
	router.current = candidate
	router.mu.Unlock()
	router.changed.signal()
	return nil
}

func (router *boundaryRouter) resume(
	predecessor *boundaryGeneration, ingressEpoch, egressEpoch *routeEpoch,
) {
	router.mu.Lock()
	defer router.mu.Unlock()
	if router.current != predecessor {
		return
	}
	if router.closed {
		// Queue shutdown can race any pre-publication abort. Ingress remains
		// permanently closed, but predecessor egress must reopen so buffered
		// terminal output stays drainable.
		if router.drainableEgress && egressEpoch != nil {
			predecessor.egressAdmission.resume(egressEpoch)
			router.changed.signal()
		}
		return
	}
	if egressEpoch != nil {
		predecessor.egressAdmission.resume(egressEpoch)
	}
	if ingressEpoch != nil {
		predecessor.ingressAdmission.resume(ingressEpoch)
	}
	router.changed.signal()
}

// close permanently closes boundary admission. It is concurrent and
// idempotent; every caller may use its own context to bound lease drainage.
func (router *boundaryRouter) close(ctx context.Context) error {
	if router == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close graph boundary router: nil context")
	}
	router.mu.Lock()
	if !router.closed {
		router.closed = true
		close(router.closedCh)
	}
	router.drainableEgress = false
	router.queueShutdown.Store(false)
	generation := router.current
	router.mu.Unlock()
	router.changed.signal()

	_, ingressErr := generation.ingressAdmission.retire(ctx)
	_, egressErr := generation.egressAdmission.retire(ctx)
	return errors.Join(ingressErr, egressErr)
}

// beginQueueShutdown permanently closes ingress while preserving the existing
// closed-queue drain contract for egress. shutdownResources closes concrete
// queues only after lifecycle-owned producers have stopped; closed queues wake
// blocked receivers while still allowing already-buffered terminal items to
// be consumed.
func (router *boundaryRouter) beginQueueShutdown(ctx context.Context) error {
	if router == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("begin graph boundary queue shutdown: nil context")
	}
	router.mu.Lock()
	if !router.closed {
		router.closed = true
		router.drainableEgress = true
		router.queueShutdown.Store(true)
		close(router.closedCh)
	}
	generation := router.current
	router.mu.Unlock()
	router.changed.signal()
	generation.ingressAdmission.stop()
	return context.Cause(ctx)
}

// waitIngressShutdown is deliberately separate from admission closure. A
// committed operation may still be inside a synchronous tracer or observer;
// lifecycle disposal must be allowed to release that callback before shutdown
// waits for its route lease.
func (router *boundaryRouter) waitIngressShutdown(ctx context.Context) error {
	if router == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("wait for graph boundary ingress shutdown: nil context")
	}
	router.mu.Lock()
	generation := router.current
	router.mu.Unlock()
	_, err := generation.ingressAdmission.retire(ctx)
	return err
}

// finishQueueShutdown retires egress once every exported output queue is both
// closed and empty. If terminal items remain, egress stays admitted so a
// caller holding (or later using) a stable port can drain them exactly as it
// could before generation indirection. The final successful/EOF receive calls
// this method again and closes admission when the last item is gone.
func (router *boundaryRouter) finishQueueShutdown(ctx context.Context, wait bool) error {
	if router == nil {
		return nil
	}
	router.mu.Lock()
	if !router.closed || !router.drainableEgress {
		router.mu.Unlock()
		return nil
	}
	generation := router.current
	for _, route := range generation.egress {
		for _, queue := range route.port.queues {
			queue.mu.Lock()
			drained := queue.closed && queue.size == 0
			queue.mu.Unlock()
			if !drained {
				router.mu.Unlock()
				return nil
			}
		}
	}
	router.drainableEgress = false
	router.queueShutdown.Store(false)
	router.mu.Unlock()
	router.changed.signal()
	_, drained := generation.egressAdmission.stop()
	if !wait {
		return nil
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	select {
	case <-drained:
		return context.Cause(ctx)
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type stableOutputPort struct {
	router   *boundaryRouter
	contract boundaryPortContract
	lanes    []element.Sender
}

func (port *stableOutputPort) Name() string       { return port.contract.name }
func (port *stableOutputPort) Type() element.Type { return port.contract.typ.Clone() }
func (port *stableOutputPort) Lanes() []element.Sender {
	return append([]element.Sender(nil), port.lanes...)
}

func (port *stableOutputPort) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	if ctx == nil {
		return element.SendResult{}, fmt.Errorf("broadcast on %s: nil context", port.contract.name)
	}
	if err := envelope.ValidateFor(port.contract.typ); err != nil {
		return element.SendResult{}, err
	}
	for {
		generation, lease, err := port.router.acquire(ctx, ir.InputBoundary)
		if err != nil {
			return element.SendResult{}, err
		}
		result, sendErr := func() (element.SendResult, error) {
			defer lease.release()
			route, found := generation.ingress[port.contract.name]
			if !found {
				return element.SendResult{}, fmt.Errorf(
					"current generation has no graph input boundary %q", port.contract.name,
				)
			}
			return route.port.broadcastWithLease(ctx, envelope, lease)
		}()
		if sendErr == errBoundaryGenerationRetired && result == (element.SendResult{}) {
			continue
		}
		return result, sendErr
	}
}

type stableSender struct {
	router   *boundaryRouter
	boundary string
	id       string
	typ      element.Type
}

func (sender *stableSender) ID() string         { return sender.id }
func (sender *stableSender) Type() element.Type { return sender.typ.Clone() }
func (sender *stableSender) Send(
	ctx context.Context, envelope element.Envelope,
) (element.DeliveryResult, error) {
	if ctx == nil {
		return "", fmt.Errorf("send on %s: nil context", sender.id)
	}
	if err := envelope.ValidateFor(sender.typ); err != nil {
		return "", err
	}
	for {
		generation, lease, err := sender.router.acquire(ctx, ir.InputBoundary)
		if err != nil {
			return "", err
		}
		result, sendErr := func() (element.DeliveryResult, error) {
			defer lease.release()
			route, found := generation.ingress[sender.boundary]
			if !found {
				return "", fmt.Errorf("current generation has no graph input boundary %q", sender.boundary)
			}
			lane, found := route.lanes[sender.id]
			if !found {
				return "", fmt.Errorf(
					"current generation graph input boundary %q has no lane %q", sender.boundary, sender.id,
				)
			}
			return lane.sendWithLease(ctx, envelope, lease)
		}()
		if sendErr == errBoundaryGenerationRetired && result == "" {
			continue
		}
		return result, sendErr
	}
}

type stableInputPort struct {
	router   *boundaryRouter
	contract boundaryPortContract
	lanes    []element.Receiver
}

func (port *stableInputPort) Name() string       { return port.contract.name }
func (port *stableInputPort) Type() element.Type { return port.contract.typ.Clone() }
func (port *stableInputPort) Lanes() []element.Receiver {
	return append([]element.Receiver(nil), port.lanes...)
}

func (port *stableInputPort) Receive(ctx context.Context) (element.Envelope, error) {
	if len(port.lanes) == 0 {
		return element.Envelope{}, fmt.Errorf("input %s: %w", port.contract.name, ErrPortUnbound)
	}
	if len(port.lanes) != 1 {
		return element.Envelope{}, fmt.Errorf("input %s has %d lanes: %w",
			port.contract.name, len(port.lanes), ErrPortCardinality)
	}
	return port.receive(ctx)
}

func (port *stableInputPort) ReceiveAny(
	ctx context.Context,
) (element.Envelope, string, error) {
	if ctx == nil {
		return element.Envelope{}, "", fmt.Errorf("receive on %s: nil context", port.contract.name)
	}
	if len(port.lanes) == 0 {
		return element.Envelope{}, "", fmt.Errorf("input %s: %w", port.contract.name, ErrPortUnbound)
	}
	for {
		generation, lease, err := port.router.acquire(ctx, ir.OutputBoundary)
		if err != nil {
			return element.Envelope{}, "", err
		}
		envelope, lane, receiveErr := func() (element.Envelope, string, error) {
			defer lease.release()
			route, found := generation.egress[port.contract.name]
			if !found {
				return element.Envelope{}, "", fmt.Errorf(
					"current generation has no graph output boundary %q", port.contract.name,
				)
			}
			return route.port.receiveAnyWithLease(ctx, lease)
		}()
		if receiveErr == errBoundaryGenerationRetired && envelope.ItemID == "" && lane == "" {
			continue
		}
		if port.router.queueShutdown.Load() {
			_ = port.router.finishQueueShutdown(context.Background(), false)
		}
		return envelope, lane, receiveErr
	}
}

func (port *stableInputPort) receive(ctx context.Context) (element.Envelope, error) {
	if ctx == nil {
		return element.Envelope{}, fmt.Errorf("receive on %s: nil context", port.contract.laneIDs[0])
	}
	for {
		generation, lease, err := port.router.acquire(ctx, ir.OutputBoundary)
		if err != nil {
			return element.Envelope{}, err
		}
		envelope, receiveErr := func() (element.Envelope, error) {
			defer lease.release()
			route, found := generation.egress[port.contract.name]
			if !found {
				return element.Envelope{}, fmt.Errorf(
					"current generation has no graph output boundary %q", port.contract.name,
				)
			}
			return route.port.receiveWithLease(ctx, lease)
		}()
		if receiveErr == errBoundaryGenerationRetired && envelope.ItemID == "" {
			continue
		}
		if port.router.queueShutdown.Load() {
			_ = port.router.finishQueueShutdown(context.Background(), false)
		}
		return envelope, receiveErr
	}
}

type stableReceiver struct {
	router   *boundaryRouter
	boundary string
	id       string
	typ      element.Type
}

func (receiver *stableReceiver) ID() string         { return receiver.id }
func (receiver *stableReceiver) Type() element.Type { return receiver.typ.Clone() }
func (receiver *stableReceiver) Receive(ctx context.Context) (element.Envelope, error) {
	if ctx == nil {
		return element.Envelope{}, fmt.Errorf("receive on %s: nil context", receiver.id)
	}
	for {
		generation, lease, err := receiver.router.acquire(ctx, ir.OutputBoundary)
		if err != nil {
			return element.Envelope{}, err
		}
		envelope, receiveErr := func() (element.Envelope, error) {
			defer lease.release()
			route, found := generation.egress[receiver.boundary]
			if !found {
				return element.Envelope{}, fmt.Errorf(
					"current generation has no graph output boundary %q", receiver.boundary,
				)
			}
			lane, found := route.lanes[receiver.id]
			if !found {
				return element.Envelope{}, fmt.Errorf(
					"current generation graph output boundary %q has no lane %q", receiver.boundary, receiver.id,
				)
			}
			return lane.receiveWithLease(ctx, lease)
		}()
		if receiveErr == errBoundaryGenerationRetired && envelope.ItemID == "" {
			continue
		}
		if receiver.router.queueShutdown.Load() {
			_ = receiver.router.finishQueueShutdown(context.Background(), false)
		}
		return envelope, receiveErr
	}
}
