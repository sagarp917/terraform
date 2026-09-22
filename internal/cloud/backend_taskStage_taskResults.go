// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package cloud

import (
	"context"
	"fmt"
	"unicode"

	"github.com/hashicorp/go-tfe"
)

type taskResultSummary struct {
	unreachable     bool
	pending         int
	failed          int
	failedMandatory int
	passed          int
}

func isNonTerminalTaskResultStatus(status tfe.TaskResultStatus) bool {
	return status == tfe.TaskRunning || status == tfe.TaskPending
}

func partitionTaskResults(taskResults []*tfe.TaskResult) ([]*tfe.TaskResult, []*tfe.TaskResult) {
	completed := make([]*tfe.TaskResult, 0, len(taskResults))
	pending := make([]*tfe.TaskResult, 0, len(taskResults))
	for _, taskResult := range taskResults {
		if isNonTerminalTaskResultStatus(taskResult.Status) {
			pending = append(pending, taskResult)
			continue
		}
		completed = append(completed, taskResult)
	}

	return completed, pending
}

type taskResultSummarizer struct {
	finished bool
	cloud    *Cloud
	counter  int
	// nativeCache memoizes hasNativeCLISummary results keyed by task result ID.
	// The outcomes endpoint is called at most once per task result instead of
	// on every poll tick.
	nativeCache map[string]bool
}

func newTaskResultSummarizer(b *Cloud, ts *tfe.TaskStage) taskStageSummarizer {
	if len(ts.TaskResults) == 0 {
		return nil
	}
	return &taskResultSummarizer{
		cloud:       b,
		nativeCache: make(map[string]bool),
	}
}

func (trs *taskResultSummarizer) Summarize(ctx *IntegrationContext, output IntegrationOutputWriter, ts *tfe.TaskStage) (bool, *string, error) {
	if trs.finished {
		return false, nil, nil
	}
	trs.counter++

	counts := summarizeTaskResults(ts.TaskResults)

	if counts.pending != 0 && !isTerminalTaskStageStatus(ts.Status) {
		pendingMessage := "%d tasks still pending, %d passed, %d failed ... "
		message := fmt.Sprintf(pendingMessage, counts.pending, counts.passed, counts.failed)
		return true, &message, nil
	}

	if counts.pending != 0 {
		completed, pending := partitionTaskResults(ts.TaskResults)
		if len(completed) == 0 {
			output.Output("Skipping task results.")
			output.End()
			return false, nil, nil
		}

		completedCounts := summarizeTaskResults(completed)
		trs.runTasksWithTaskResults(ctx.StopContext, output, completed, completedCounts)
		output.Output(fmt.Sprintf("Skipping %d pending task result(s) because task stage is %s.", len(pending), ts.Status))
		output.End()
		trs.finished = true
		return false, nil, nil
	}

	if counts.unreachable {
		output.Output("Skipping task results.")
		output.End()
		return false, nil, nil
	}

	// Print out the summary
	trs.runTasksWithTaskResults(ctx.StopContext, output, ts.TaskResults, counts)

	// Mark as finished
	trs.finished = true

	return false, nil, nil
}

func summarizeTaskResults(taskResults []*tfe.TaskResult) *taskResultSummary {
	var pendingCount, errCount, errMandatoryCount, passedCount int
	for _, task := range taskResults {
		if task.Status == tfe.TaskUnreachable {
			return &taskResultSummary{
				unreachable: true,
			}
		} else if task.Status == tfe.TaskRunning || task.Status == tfe.TaskPending {
			pendingCount++
		} else if task.Status == tfe.TaskPassed {
			passedCount++
		} else {
			// Everything else is a failure
			errCount++
			if task.WorkspaceTaskEnforcementLevel == tfe.Mandatory {
				errMandatoryCount++
			}
		}
	}

	return &taskResultSummary{
		unreachable:     false,
		pending:         pendingCount,
		failed:          errCount,
		failedMandatory: errMandatoryCount,
		passed:          passedCount,
	}
}

func (trs *taskResultSummarizer) runTasksWithTaskResults(ctx context.Context, output IntegrationOutputWriter, taskResults []*tfe.TaskResult, count *taskResultSummary) {
	// Track the first task name that is a mandatory enforcement level breach.
	var firstMandatoryTaskFailed *string = nil

	if trs.counter == 0 {
		output.Output(fmt.Sprintf("All tasks completed! %d passed, %d failed", count.passed, count.failed))
	} else {
		output.OutputElapsed(fmt.Sprintf("All tasks completed! %d passed, %d failed", count.passed, count.failed), 50)
	}

	output.Output("")

	// renderedAny tracks whether at least one non-native task row was printed.
	// If every task in the stage is a native task, nativeTaskSummarizer owns
	// the per-task blocks, so we suppress the generic overall-result footer.
	renderedAny := false
	for _, t := range taskResults {
		// Native tasks own their own CLI output block; skip them here so
		// nativeTaskSummarizer can render the full INSIGHTS display.
		// isNative is memoized: the outcomes endpoint is called at most once
		// per task result ID across all poll ticks.
		isNative, ok := trs.nativeCache[t.ID]
		if !ok {
			isNative = trs.cloud.hasNativeCLISummary(ctx, t.ID)
			trs.nativeCache[t.ID] = isNative
		}
		if isNative {
			if t.Status != "passed" && t.WorkspaceTaskEnforcementLevel == "mandatory" && firstMandatoryTaskFailed == nil {
				firstMandatoryTaskFailed = &t.TaskName
			}
			continue
		}

		renderedAny = true
		capitalizedStatus := capitalize(string(t.Status))

		status := "[green]" + capitalizedStatus
		if t.Status != "passed" {
			status = fmt.Sprintf("[red]%s (%s)", capitalizedStatus, capitalize(string(t.WorkspaceTaskEnforcementLevel)))

			if t.WorkspaceTaskEnforcementLevel == "mandatory" && firstMandatoryTaskFailed == nil {
				firstMandatoryTaskFailed = &t.TaskName
			}
		}

		title := fmt.Sprintf(`%s ⸺   %s`, t.TaskName, status)
		output.SubOutput(title)

		if len(t.Message) > 0 {
			output.SubOutput(fmt.Sprintf("[dim]%s", t.Message))
		}
		if len(t.URL) > 0 {
			output.SubOutput(fmt.Sprintf("[dim]Details: %s", t.URL))
		}
		output.SubOutput("")
	}

	// Every task was a native task — nativeTaskSummarizer owns the output.
	// Skip the generic footer so there's no orphaned "Overall Result" line.
	if !renderedAny {
		return
	}

	// If a mandatory enforcement level is breached, return an error.
	var overall string = "[green]Passed"
	if firstMandatoryTaskFailed != nil {
		overall = "[red]Failed"
		if count.failedMandatory > 1 {
			output.Output(fmt.Sprintf("[reset][bold][red]Error:[reset][bold]the run failed because %d mandatory tasks are required to succeed", count.failedMandatory))
		} else {
			output.Output(fmt.Sprintf("[reset][bold][red]Error: [reset][bold]the run failed because the run task, %s, is required to succeed", *firstMandatoryTaskFailed))
		}
	} else if count.failed > 0 { // we have failures but none of them mandatory
		overall = "[green]Passed with advisory failures"
	}

	output.SubOutput("")
	output.SubOutput("[bold]Overall Result: " + overall)

	output.End()
}

// capitalize upper-cases the first rune of s and returns the result.
// Returns s unchanged if s is empty.
func capitalize(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
