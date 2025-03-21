// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package local

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform/internal/backend/backendrun"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/states/statemgr"
)

func TestLocal_impl(t *testing.T) {
	var _ backendrun.OperationsBackend = New()
	var _ backendrun.Local = New()
	var _ backendrun.CLI = New()
}

func checkState(t *testing.T, path, expected string) {
	t.Helper()
	// Read the state
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	state, err := statefile.Read(f)
	f.Close()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	actual := state.State.String()
	expected = strings.TrimSpace(expected)
	if actual != expected {
		t.Fatalf("state does not match! actual:\n%s\n\nexpected:\n%s", actual, expected)
	}
}

// a local backend which returns sentinel errors for NamedState methods to
// verify it's being called.
type testDelegateBackend struct {
	*Local

	// return a sentinel error on these calls
	stateErr  bool
	statesErr bool
	deleteErr bool
}

var errTestDelegateState = errors.New("state called")
var errTestDelegateStates = errors.New("states called")
var errTestDelegateDeleteState = errors.New("delete called")

func (b *testDelegateBackend) StateMgr(name string) (statemgr.Full, error) {
	if b.stateErr {
		return nil, errTestDelegateState
	}
	s := statemgr.NewFilesystem("terraform.tfstate")
	return s, nil
}

func (b *testDelegateBackend) Workspaces() ([]string, error) {
	if b.statesErr {
		return nil, errTestDelegateStates
	}
	return []string{"default"}, nil
}

func (b *testDelegateBackend) DeleteWorkspace(name string, force bool) error {
	if b.deleteErr {
		return errTestDelegateDeleteState
	}
	return nil
}

// verify that the MultiState methods are dispatched to the correct Backend.
func TestLocal_multiStateBackend(t *testing.T) {
	// assign a separate backend where we can read the state
	b, _ := NewWithBackend(&testDelegateBackend{
		stateErr:  true,
		statesErr: true,
		deleteErr: true,
	})

	if _, err := b.StateMgr("test"); err != errTestDelegateState {
		t.Fatal("expected errTestDelegateState, got:", err)
	}

	if _, err := b.Workspaces(); err != errTestDelegateStates {
		t.Fatal("expected errTestDelegateStates, got:", err)
	}

	if err := b.DeleteWorkspace("test", true); err != errTestDelegateDeleteState {
		t.Fatal("expected errTestDelegateDeleteState, got:", err)
	}
}
