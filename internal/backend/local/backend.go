// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package local

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/backend/backendrun"
	localState "github.com/hashicorp/terraform/internal/backend/local-state"
	"github.com/hashicorp/terraform/internal/command/views"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/logging"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

const (
	DefaultWorkspaceDir    = "terraform.tfstate.d"
	DefaultWorkspaceFile   = "environment"
	DefaultStateFilename   = "terraform.tfstate"
	DefaultBackupExtension = ".backup"
)

var (
	MissingStateStorageDiag = hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Missing state store",
		Detail:   "Attempted to call the PrepareConfig method on a missing state store. This is a bug in Terraform and should be reported",
	}
)

// Local is an implementation of OperationsBackend that performs all operations
// locally. This is the "default" backend and implements normal Terraform
// behavior as it is well known.
type Local struct {
	// We only want to create a single instance of a local state, so store them
	// here as they're loaded.
	states map[string]statemgr.Full

	// Terraform context. Many of these will be overridden or merged by
	// Operation. See Operation for more details.
	ContextOpts *terraform.ContextOpts

	// OpInput will ask for necessary input prior to performing any operations.
	//
	// OpValidation will perform validation prior to running an operation. The
	// variable naming doesn't match the style of others since we have a func
	// Validate.
	OpInput      bool
	OpValidation bool

	// Backend, if non-nil, will use this backend for non-enhanced behavior.
	// This allows local behavior with remote state storage. It is a way to
	// "upgrade" a non-enhanced backend to an enhanced backend with typical
	// behavior.
	//
	// If this is nil, local performs normal state loading and storage.
	Backend backend.Backend

	// opLock locks operations
	opLock sync.Mutex
}

var _ backend.Backend = (*Local)(nil)
var _ backendrun.OperationsBackend = (*Local)(nil)

// New returns a new initialized local backend.
// As no backend was supplied by the caller (used New instead of NewWithBackend)
// an instance of the local state backend is provided as default.
func New() *Local {
	backend := localState.New()
	b, err := NewWithBackend(backend)
	if err != nil {
		// We cannot return an error here
		panic(fmt.Sprintf("error encountered in backendLocal.New. This is a bug in Terraform and should be reported. Error: %s", err))
	}
	return b
}

// NewWithBackend returns a new local backend initialized with a
// dedicated backend for non-enhanced behavior.
func NewWithBackend(backend backend.Backend) (*Local, error) {
	if backend == nil {
		return nil, fmt.Errorf("nil backend.Backend pointer received when creating a local backend. This is an error in Terraform and should be reported.")
	}
	return &Local{
		Backend: backend,
	}, nil
}

func (b *Local) ConfigSchema() *configschema.Block {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		return &configschema.Block{}
	}

	return b.Backend.ConfigSchema()
}

func (b *Local) PrepareConfig(obj cty.Value) (cty.Value, tfdiags.Diagnostics) {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		var diags tfdiags.Diagnostics
		diags = diags.Append(MissingStateStorageDiag)
		return cty.NullVal(cty.EmptyObject), diags
	}

	return b.Backend.PrepareConfig(obj)
}

func (b *Local) Configure(obj cty.Value) tfdiags.Diagnostics {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		var diags tfdiags.Diagnostics
		return diags.Append(MissingStateStorageDiag)
	}

	return b.Backend.Configure(obj)
}

func (b *Local) ServiceDiscoveryAliases() ([]backendrun.HostAlias, error) {
	return []backendrun.HostAlias{}, nil
}

func (b *Local) Workspaces() ([]string, error) {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		return nil, fmt.Errorf("%s", MissingStateStorageDiag.Error())
	}

	return b.Backend.Workspaces()
}

// DeleteWorkspace removes a workspace.
//
// The "default" workspace cannot be removed.
func (b *Local) DeleteWorkspace(name string, force bool) error {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		return fmt.Errorf("%s", MissingStateStorageDiag.Error())
	}

	return b.Backend.DeleteWorkspace(name, force)
}

func (b *Local) StateMgr(name string) (statemgr.Full, error) {
	if b.Backend == nil {
		// The local operations backend doesn't have a state storage backend
		return nil, fmt.Errorf("%s", MissingStateStorageDiag.Error())
	}

	return b.Backend.StateMgr(name)
}

// Operation implements backendrun.OperationsBackend
//
// This will initialize an in-memory terraform.Context to perform the
// operation within this process.
//
// The given operation parameter will be merged with the ContextOpts on
// the structure with the following rules. If a rule isn't specified and the
// name conflicts, assume that the field is overwritten if set.
func (b *Local) Operation(ctx context.Context, op *backendrun.Operation) (*backendrun.RunningOperation, error) {
	if op.View == nil {
		panic("Operation called with nil View")
	}

	// Determine the function to call for our operation
	var f func(context.Context, context.Context, *backendrun.Operation, *backendrun.RunningOperation)
	switch op.Type {
	case backendrun.OperationTypeRefresh:
		f = b.opRefresh
	case backendrun.OperationTypePlan:
		f = b.opPlan
	case backendrun.OperationTypeApply:
		f = b.opApply
	default:
		return nil, fmt.Errorf(
			"unsupported operation type: %s\n\n"+
				"This is a bug in Terraform and should be reported. The local backend\n"+
				"is built-in to Terraform and should always support all operations.",
			op.Type)
	}

	// Lock
	b.opLock.Lock()

	// Build our running operation
	// the runninCtx is only used to block until the operation returns.
	runningCtx, done := context.WithCancel(context.Background())
	runningOp := &backendrun.RunningOperation{
		Context: runningCtx,
	}

	// stopCtx wraps the context passed in, and is used to signal a graceful Stop.
	stopCtx, stop := context.WithCancel(ctx)
	runningOp.Stop = stop

	// cancelCtx is used to cancel the operation immediately, usually
	// indicating that the process is exiting.
	cancelCtx, cancel := context.WithCancel(context.Background())
	runningOp.Cancel = cancel

	op.StateLocker = op.StateLocker.WithContext(stopCtx)

	// Do it
	go func() {
		defer logging.PanicHandler()
		defer done()
		defer stop()
		defer cancel()

		defer b.opLock.Unlock()
		f(stopCtx, cancelCtx, op, runningOp)
	}()

	// Return
	return runningOp, nil
}

// opWait waits for the operation to complete, and a stop signal or a
// cancelation signal.
func (b *Local) opWait(
	doneCh <-chan struct{},
	stopCtx context.Context,
	cancelCtx context.Context,
	tfCtx *terraform.Context,
	opStateMgr statemgr.Persister,
	view views.Operation) (canceled bool) {
	// Wait for the operation to finish or for us to be interrupted so
	// we can handle it properly.
	select {
	case <-stopCtx.Done():
		view.Stopping()

		// try to force a PersistState just in case the process is terminated
		// before we can complete.
		if err := opStateMgr.PersistState(nil); err != nil {
			// We can't error out from here, but warn the user if there was an error.
			// If this isn't transient, we will catch it again below, and
			// attempt to save the state another way.
			var diags tfdiags.Diagnostics
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Error saving current state",
				fmt.Sprintf(earlyStateWriteErrorFmt, err),
			))
			view.Diagnostics(diags)
		}

		// Stop execution
		log.Println("[TRACE] backend/local: waiting for the running operation to stop")
		go tfCtx.Stop()

		select {
		case <-cancelCtx.Done():
			log.Println("[WARN] running operation was forcefully canceled")
			// if the operation was canceled, we need to return immediately
			canceled = true
		case <-doneCh:
			log.Println("[TRACE] backend/local: graceful stop has completed")
		}
	case <-cancelCtx.Done():
		// this should not be called without first attempting to stop the
		// operation
		log.Println("[ERROR] running operation canceled without Stop")
		canceled = true
	case <-doneCh:
	}
	return
}

const earlyStateWriteErrorFmt = `Error: %s

Terraform encountered an error attempting to save the state before cancelling the current operation. Once the operation is complete another attempt will be made to save the final state.`
