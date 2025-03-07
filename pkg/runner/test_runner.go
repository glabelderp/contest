// Copyright (c) Facebook, Inc. and its affiliates.
//
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/insomniacslk/xjson"

	"github.com/linuxboot/contest/pkg/cerrors"
	"github.com/linuxboot/contest/pkg/config"
	"github.com/linuxboot/contest/pkg/event/testevent"
	"github.com/linuxboot/contest/pkg/target"
	"github.com/linuxboot/contest/pkg/test"
	"github.com/linuxboot/contest/pkg/xcontext"
)

// TestRunner is the state associated with a test run.
// Here's how a test run works:
//  * Each target gets a targetState and a "target handler" - a goroutine that takes that particular
//    target through each step of the pipeline in sequence. It injects the target, waits for the result,
//    then moves on to the next step.
//  * Each step of the pipeline gets a stepState and:
//    - A "step runner" - a goroutine that is responsible for running the step's Run() method
//    - A "step reader" - a goroutine that processes results and sends them on to target handlers that await them.
//  * After starting all of the above, the main goroutine goes into "monitor" mode
//    that checks on the pipeline's progress and is responsible for closing step input channels
//    when all the targets have been injected.
//  * Monitor loop finishes when all the targets have been injected into the last step
//    or if a step has encountered an error.
//  * We then wait for all the step runners and readers to shut down.
//  * Once all the activity has died down, resulting state is examined and an error is returned, if any.
type TestRunner struct {
	shutdownTimeout time.Duration // Time to wait for steps runners to finish a the end of the run

	steps []*stepState // The pipeline, in order of execution

	// One mutex to rule them all, used to serialize access to all the state above.
	// Could probably be split into several if necessary.
	mu sync.Mutex
}

// targetStepPhase denotes progression of a target through a step
type targetStepPhase int

const (
	targetStepPhaseInvalid  targetStepPhase = iota
	targetStepPhaseInit                     // (1) Created
	targetStepPhaseBegin                    // (2) Picked up for execution.
	targetStepPhaseRun                      // (3) Injected into step.
	targetStepPhaseObsolete                 // (4) Former result posted to the handler. [Obsolete]
	targetStepPhaseEnd                      // (5) Finished running a step.
)

// stepVariables represents the emitted variables of the steps
type stepVariables map[string]json.RawMessage

// targetState contains state associated with one target progressing through the pipeline.
type targetState struct {
	tgt *target.Target

	// This part of state gets serialized into JSON for resumption.
	CurStep        int                      `json:"S,omitempty"` // Current step number.
	CurPhase       targetStepPhase          `json:"P,omitempty"` // Current phase of step execution.
	Res            *xjson.Error             `json:"R,omitempty"` // Final result, if reached the end state.
	StepsVariables map[string]stepVariables `json:"V,omitempty"` // maps steps onto emitted variables of each
}

// resumeStateStruct is used to serialize runner state to be resumed in the future.
type resumeStateStruct struct {
	Version         int                     `json:"V"`
	Targets         map[string]*targetState `json:"T"`
	StepResumeState []json.RawMessage       `json:"SRS,omitempty"`
}

// Resume state version we are compatible with.
// When incompatible changes are made to the state format, bump this.
// Restoring incompatible state will abort the job.
const resumeStateStructVersion = 2

type TestStepEventsEmitterFactory interface {
	New(testStepLabel string) testevent.Emitter
}

// Run is the main entry point of the runner.
func (tr *TestRunner) Run(
	ctx xcontext.Context,
	t *test.Test, targets []*target.Target,
	emitterFactory TestStepEventsEmitterFactory,
	resumeState json.RawMessage,
) (json.RawMessage, map[string]error, error) {

	// Peel off contexts used for steps and target handlers.
	runCtx, runCancel := xcontext.WithCancel(ctx)
	defer runCancel()

	var targetStates map[string]*targetState

	// If we have state to resume, parse it.
	var rs resumeStateStruct
	if len(resumeState) > 0 {
		ctx.Debugf("Attempting to resume from state: %s", string(resumeState))
		if err := json.Unmarshal(resumeState, &rs); err != nil {
			return nil, nil, fmt.Errorf("invalid resume state: %w", err)
		}
		if rs.Version != resumeStateStructVersion {
			return nil, nil, fmt.Errorf("incompatible resume state version %d (want %d)",
				rs.Version, resumeStateStructVersion)
		}
		targetStates = rs.Targets
	}

	// Set up the targets
	if targetStates == nil {
		targetStates = make(map[string]*targetState)
	}

	// Initialize remaining fields of the target structures,
	// build the map and kick off target processing.
	for _, tgt := range targets {
		tgs := targetStates[tgt.ID]
		if tgs == nil {
			tgs = &targetState{
				CurPhase: targetStepPhaseInit,
			}
		}
		tgs.tgt = tgt
		targetStates[tgt.ID] = tgs
	}

	stepOutputs, err := newTestStepsVariables(t.TestStepsBundles)
	if err != nil {
		ctx.Errorf("Failed to initialise test steps variables: %v", err)
		return nil, nil, err
	}

	for targetID, targetState := range targetStates {
		if err := stepOutputs.initTargetStepsVariables(targetID, targetState.StepsVariables); err != nil {
			ctx.Errorf("Failed to initialise test steps variables for target: %s: %v", targetID, err)
			return nil, nil, err
		}
	}

	// Set up the pipeline
	stepsErrorsCh := make(chan error, len(t.TestStepsBundles))
	for i, sb := range t.TestStepsBundles {
		var srs json.RawMessage
		if i < len(rs.StepResumeState) && string(rs.StepResumeState[i]) != "null" {
			srs = rs.StepResumeState[i]
		}

		// Collect "processed" targets in resume state for a StepRunner
		var resumeStateTargets []target.Target

		var stepTargetsCount int
		for _, tgt := range targetStates {
			if tgt.CurStep <= i {
				stepTargetsCount++
			}

			if tgt.CurStep == i && tgt.CurPhase == targetStepPhaseRun {
				resumeStateTargets = append(resumeStateTargets, *tgt.tgt)
			}
		}

		// Step handlers will be started from target handlers as targets reach them.
		tr.steps = append(tr.steps, newStepState(i, stepTargetsCount, sb, emitterFactory, stepOutputs, srs, resumeStateTargets, func(err error) {
			stepsErrorsCh <- err
		}))
	}

	targetErrors := make(chan error, len(targetStates))
	targetErrorsCount := int32(len(targetStates))
	for _, tgs := range targetStates {
		go func(ctx xcontext.Context, state *targetState, targetErrors chan<- error) {
			targetErr := tr.handleTarget(ctx, state)
			if targetErr != nil {
				runCtx.Errorf("Target %s reported an error: %v", state.tgt.ID, targetErr)
			}
			targetErrors <- targetErr
			if atomic.AddInt32(&targetErrorsCount, -1) == 0 {
				close(targetErrors)
			}
		}(runCtx, tgs, targetErrors)
	}

	runErr := func() error {
		var resultErr error
		for {
			var (
				runErr error
				ok     bool
			)
			select {
			case runErr, ok = <-targetErrors:
				if !ok {
					return resultErr
				}
			case runErr = <-stepsErrorsCh:
			}
			if runErr != nil && runErr != xcontext.ErrPaused && resultErr == nil {
				resultErr = runErr

				ctx.Errorf("Got error: %v, canceling", runErr)
				for _, ss := range tr.steps {
					ss.ForceStop()
				}
			}
		}
	}()

	// Wait for step runners and readers to exit.
	stepResumeStates, err := tr.waitSteps(ctx)
	if err != nil && runErr == nil {
		runErr = err
	}

	// There will be no more results cancel everything
	ctx.Debugf("cancel target handlers")
	runCancel()

	// Has the run been canceled? If so, ignore whatever happened, it doesn't matter.
	select {
	case <-ctx.Done():
		runErr = xcontext.ErrCanceled
	default:
	}

	// Examine the resulting state.
	ctx.Debugf("leaving, err %v, target states:", runErr)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	resumeOk := runErr == nil
	numInFlightTargets := 0
	for i, tgt := range targets {
		tgs := targetStates[tgt.ID]
		tgs.StepsVariables, err = stepOutputs.getTargetStepsVariables(tgt.ID)
		if err != nil {
			ctx.Errorf("Failed to get steps variables: %v", err)
			return nil, nil, err
		}
		stepErr := tr.steps[tgs.CurStep].GetError()
		if tgs.CurPhase == targetStepPhaseRun {
			numInFlightTargets++
			if stepErr != xcontext.ErrPaused {
				resumeOk = false
			}
		}
		if stepErr != nil && stepErr != xcontext.ErrPaused {
			resumeOk = false
		}
		ctx.Debugf("  %d target: '%s' step err: '%v', resume ok: '%t'", i, tgs, stepErr, resumeOk)
	}
	ctx.Debugf("- %d in flight, ok to resume? %t", numInFlightTargets, resumeOk)
	ctx.Debugf("step states:")
	for i, ss := range tr.steps {
		ctx.Debugf("  %d %s %t %t %s", i, ss, ss.stepRunner.Started(), ss.GetError(), stepResumeStates[i])
	}

	// Is there a useful error to report?
	if runErr != nil {
		return nil, nil, runErr
	}

	// Have we been asked to pause? If yes, is it safe to do so?
	select {
	case <-ctx.Until(xcontext.ErrPaused):
		if !resumeOk {
			ctx.Warnf("paused but not ok to resume")
			break
		}
		rs := &resumeStateStruct{
			Version:         resumeStateStructVersion,
			Targets:         targetStates,
			StepResumeState: stepResumeStates,
		}
		resumeState, runErr = json.Marshal(rs)
		if runErr != nil {
			ctx.Errorf("unable to serialize the state: %s", runErr)
			return nil, nil, runErr
		}
		ctx.Debugf("resume state: %s", resumeState)
		runErr = xcontext.ErrPaused
	default:
	}

	targetsResults := make(map[string]error)
	for id, state := range targetStates {
		if state.Res != nil {
			targetsResults[id] = state.Res.Unwrap()
		} else if state.CurStep == len(tr.steps)-1 && state.CurPhase == targetStepPhaseEnd {
			targetsResults[id] = nil
		}
	}
	return resumeState, targetsResults, runErr
}

func (tr *TestRunner) waitSteps(ctx xcontext.Context) ([]json.RawMessage, error) {
	ctx.Debugf("waiting for step runners to finish")

	shutdownCtx, cancel := context.WithTimeout(ctx, tr.shutdownTimeout)
	defer cancel()

	var stepsNeverReturned []string
	var resumeStates []json.RawMessage
	var resultErr error
	for _, ss := range tr.steps {
		if !ss.Started() {
			resumeStates = append(resumeStates, ss.GetInitResumeState())
			continue
		}
		result, err := ss.WaitResults(shutdownCtx)
		if err != nil {
			stepsNeverReturned = append(stepsNeverReturned, ss.GetTestStepLabel())
			ss.SetError(ctx, &cerrors.ErrTestStepsNeverReturned{StepNames: []string{ss.GetTestStepLabel()}})
			// Stop step context, this will help release the reader.
			ss.ForceStop()
		} else if resultErr == nil && result.Err != nil && result.Err != xcontext.ErrPaused {
			resultErr = result.Err
		}
		resumeStates = append(resumeStates, result.ResumeState)
	}

	if len(stepsNeverReturned) > 0 && resultErr == nil {
		resultErr = &cerrors.ErrTestStepsNeverReturned{StepNames: stepsNeverReturned}
	}
	return resumeStates, resultErr
}

// handleTarget takes a single target through each step of the pipeline in sequence.
// It injects the target, waits for the result, then moves on to the next step.
func (tr *TestRunner) handleTarget(ctx xcontext.Context, tgs *targetState) error {
	lastDecremented := tgs.CurStep - 1
	defer func() {
		ctx.Debugf("%s: target handler finished", tgs)

		for i := lastDecremented + 1; i < len(tr.steps); i++ {
			tr.steps[i].DecreaseLeftTargets()
		}
	}()

	ctx = ctx.WithField("target", tgs.tgt.ID)
	ctx.Debugf("%s: target handler active", tgs)
	// NB: CurStep may be non-zero on entry if resumed
loop:
	for i := tgs.CurStep; i < len(tr.steps); {
		// Early check for pause or cancellation.
		select {
		case <-ctx.Until(xcontext.ErrPaused):
			ctx.Debugf("%s: paused 0", tgs)
			break loop
		case <-ctx.Done():
			ctx.Debugf("%s: canceled 0", tgs)
			break loop
		default:
		}
		tr.mu.Lock()
		ss := tr.steps[i]
		switch tgs.CurPhase {
		case targetStepPhaseInit:
			// Normal case, inject and wait for result.
			tgs.CurPhase = targetStepPhaseBegin
		case targetStepPhaseBegin:
			// Paused before injection.
		case targetStepPhaseRun:
			// Resumed in running state, skip injection.
		case targetStepPhaseEnd:
			// Resumed in terminal state, we are done.
			tr.mu.Unlock()
			break loop
		default:
			tr.mu.Unlock()
			err := fmt.Errorf("%s: invalid phase %s", tgs, tgs.CurPhase)
			ctx.Errorf("%v", err)
			return err
		}
		tr.mu.Unlock()
		// Make sure we have a step runner active. If not, start one.
		err := ss.Run(ctx)

		var targetNotifier ChanNotifier
		if err == nil {
			// Inject the target.
			ctx.Debugf("%s: injecting into %s", tgs, ss)
			targetNotifier, err = ss.InjectTarget(ctx, tgs.tgt)
		}

		if err == nil {
			tr.mu.Lock()
			// By the time we get here the target could have been processed and result posted already, hence the check.
			if tgs.CurPhase == targetStepPhaseBegin {
				tgs.CurPhase = targetStepPhaseRun
			}
			tr.mu.Unlock()
		}

		tr.steps[i].DecreaseLeftTargets()
		lastDecremented = i

		// Await result. It will be communicated to us by the step runner
		// and returned in tgs.res.
		if err == nil {
			select {
			case res := <-targetNotifier.NotifyCh():
				ctx.Debugf("Got target result: '%v'", err)
				tr.mu.Lock()
				if res != nil {
					tgs.Res = xjson.NewError(res)
				}
				tgs.CurPhase = targetStepPhaseEnd
				tr.mu.Unlock()
				err = nil
			case <-ss.NotifyStopped():
				err = ss.GetError()
				ctx.Debugf("step runner stopped: '%v'", err)
			case <-ctx.Done():
				ctx.Debugf("Canceled target context during waiting for target result")
				err = ctx.Err()
			}
		}
		if err != nil {
			ctx.Errorf("Target handler failed: %v", err)
			switch err {
			case xcontext.ErrPaused:
				ctx.Debugf("%s: paused 1", tgs)
			case xcontext.ErrCanceled:
				ctx.Debugf("%s: canceled 1", tgs)
			}
			return err
		}

		tr.mu.Lock()
		if tgs.Res != nil {
			tr.mu.Unlock()
			break
		}
		i++
		if i < len(tr.steps) {
			tgs.CurStep = i
			tgs.CurPhase = targetStepPhaseInit
		}
		tr.mu.Unlock()
	}
	return nil
}

func NewTestRunnerWithTimeouts(shutdownTimeout time.Duration) *TestRunner {
	tr := &TestRunner{
		shutdownTimeout: shutdownTimeout,
	}
	return tr
}

func NewTestRunner() *TestRunner {
	return NewTestRunnerWithTimeouts(config.TestRunnerShutdownTimeout)
}

func (tph targetStepPhase) String() string {
	switch tph {
	case targetStepPhaseInvalid:
		return "INVALID"
	case targetStepPhaseInit:
		return "init"
	case targetStepPhaseBegin:
		return "begin"
	case targetStepPhaseRun:
		return "run"
	case targetStepPhaseObsolete:
		return "result_pending_obsolete"
	case targetStepPhaseEnd:
		return "end"
	}
	return fmt.Sprintf("???(%d)", tph)
}

func (tgs *targetState) String() string {
	var resText string
	if tgs.Res != nil {
		resStr := fmt.Sprintf("%v", tgs.Res)
		if len(resStr) > 20 {
			resStr = resStr[:20] + "..."
		}
		resText = fmt.Sprintf("%q", resStr)
	} else {
		resText = "<nil>"
	}
	return fmt.Sprintf("[%s %d %s %s]",
		tgs.tgt, tgs.CurStep, tgs.CurPhase, resText)
}
