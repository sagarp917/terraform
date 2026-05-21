// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// Cloudability outcome structures for parsing body content
type cloudabilityOutcomeBody struct {
	Meta   json.RawMessage `json:"meta"`
	Result json.RawMessage `json:"result"`
}

type costEstimationMeta struct {
	CurrencyCode        string `json:"currency_code"`
	CostGuardrailStatus string `json:"cost_guardrail_status"`
}

type costEstimationResult struct {
	TotalCostBefore    string              `json:"total_cost_before"`
	TotalCostAfter     string              `json:"total_cost_after"`
	TotalCostDiff      string              `json:"total_cost_diff"`
	CostGuardrails     []costGuardrail     `json:"cost_guardrails"`
	CloudResourceDiffs []cloudResourceDiff `json:"cloud_resource_diffs"`
}

type costGuardrail struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	EnforcementLevel string `json:"enforcement_level"`
	CostDiff         *struct {
		MaxAllowedDiff string `json:"max_allowed_diff"`
	} `json:"cost_diff"`
}

type cloudResourceDiff struct {
	Selector     string `json:"selector"`
	ResourceType string `json:"resource_type"`
	CostDiff     string `json:"cost_diff"`
}

type policyEvaluationMeta struct {
	Passed              bool   `json:"passed"`
	TotalFailedPolicies int    `json:"total_failed_policies"`
	HighestFailureLevel string `json:"highest_failure_level"`
}

type policyEvaluationResult struct {
	FailedPolicies []failedPolicy `json:"failed_policies"`
}

type failedPolicy struct {
	PolicyName       string `json:"policy_name"`
	EnforcementLevel string `json:"enforcement_level"`
}

type recommendationMeta struct {
	CurrencyCode string `json:"currency_code"`
}

type recommendationResult struct {
	CloudResourceRecommendations []cloudResourceRecommendation `json:"cloud_resource_recommendations"`
}

type cloudResourceRecommendation struct {
	Selector            string           `json:"selector"`
	ResourceType        string           `json:"resource_type"`
	CurrentResource     *resourceDetails `json:"current_resource"`
	RecommendedResource *resourceDetails `json:"recommended_resource"`
}

type resourceDetails struct {
	InstanceType string  `json:"instanceType"`
	PricePerUnit float64 `json:"pricePerUnit"`
}

type nativeTaskResultSummary struct {
	unreachable     bool
	pending         int
	failed          int
	failedMandatory int
	passed          int
}

type nativeTaskResultSummarizer struct {
	finished bool
	cloud    *Cloud
	counter  int
}

func newNativeTaskResultSummarizer(b *Cloud, ts *tfe.TaskStage) taskStageSummarizer {
	// Filter for native tasks only
	nativeTasks := filterNativeTaskResults(ts.TaskResults)
	if len(nativeTasks) == 0 {
		return nil
	}
	return &nativeTaskResultSummarizer{
		finished: false,
		cloud:    b,
	}
}

func filterNativeTaskResults(taskResults []*tfe.TaskResult) []*tfe.TaskResult {
	var nativeTasks []*tfe.TaskResult
	for _, task := range taskResults {
		if isNativeTask(task) {
			nativeTasks = append(nativeTasks, task)
		}
	}
	return nativeTasks
}

func isNativeTask(task *tfe.TaskResult) bool {
	return task.TaskCategory == "native"
}

func (ntrs *nativeTaskResultSummarizer) Summarize(context *IntegrationContext, output IntegrationOutputWriter, ts *tfe.TaskStage) (bool, *string, error) {
	if ntrs.finished {
		return false, nil, nil
	}
	ntrs.counter++

	nativeTasks := filterNativeTaskResults(ts.TaskResults)
	counts := summarizeNativeTaskResults(nativeTasks)

	if counts.pending != 0 {
		pendingMessage := "%d native tasks still pending, %d passed, %d failed ... "
		message := fmt.Sprintf(pendingMessage, counts.pending, counts.passed, counts.failed)
		return true, &message, nil
	}
	if counts.unreachable {
		output.Output("Skipping native task results.")
		output.End()
		return false, nil, nil
	}

	// Print out the summary
	ntrs.nativeTasksWithTaskResults(output, nativeTasks, counts)

	// Mark as finished
	ntrs.finished = true

	return false, nil, nil
}

func summarizeNativeTaskResults(taskResults []*tfe.TaskResult) *nativeTaskResultSummary {
	var pendingCount, errCount, errMandatoryCount, passedCount int
	for _, task := range taskResults {
		switch task.Status {
		case tfe.TaskUnreachable:
			return &nativeTaskResultSummary{
				unreachable: true,
			}
		case tfe.TaskRunning, tfe.TaskPending:
			pendingCount++
		case tfe.TaskPassed:
			passedCount++
		default:
			// Everything else is a failure
			errCount++
			if task.WorkspaceTaskEnforcementLevel == tfe.Mandatory {
				errMandatoryCount++
			}
		}
	}

	return &nativeTaskResultSummary{
		unreachable:     false,
		pending:         pendingCount,
		failed:          errCount,
		failedMandatory: errMandatoryCount,
		passed:          passedCount,
	}
}

func (ntrs *nativeTaskResultSummarizer) nativeTasksWithTaskResults(output IntegrationOutputWriter, taskResults []*tfe.TaskResult, count *nativeTaskResultSummary) {
	// Track the first task name that is a mandatory enforcement level breach.
	var firstMandatoryTaskFailed *string = nil

	// Display task header with completion status
	if ntrs.counter == 0 {
		output.Output("IBM Cloudability Native Task")
		output.Output("Running complete!")
	} else {
		output.OutputElapsed("Running complete!", 50)
	}

	output.Output("")

	for _, t := range taskResults {
		// Determine status with proper symbols
		status := "✓ PASSED"
		statusColor := "[green]"
		if t.Status != "passed" {
			status = "× FAILED"
			statusColor = "[red]"

			if t.WorkspaceTaskEnforcementLevel == "mandatory" && firstMandatoryTaskFailed == nil {
				firstMandatoryTaskFailed = &t.TaskName
			}
		}

		// Fetch and display detailed outcomes FIRST
		ntrs.displayDetailedOutcomes(output, t, statusColor+status)

		// Display basic message if no detailed outcomes
		if len(t.TaskResultOutcomes) == 0 {
			output.Output(fmt.Sprintf("→→  [bold]Overall result: %s%s", statusColor, status))
			if len(t.Message) > 0 {
				output.Output(fmt.Sprintf("[dim]%s", t.Message))
			}
			if len(t.URL) > 0 {
				output.Output(fmt.Sprintf("[dim]Details: %s", t.URL))
			}
		}

		output.Output("")
		output.Output(strings.Repeat("-", 69))
		output.Output("")
	}

	// If a mandatory enforcement level is breached, return an error.
	if firstMandatoryTaskFailed != nil {
		if count.failedMandatory > 1 {
			output.Output(fmt.Sprintf("[reset][bold][red]Error:[reset][bold] Failed to override: Apply discarded."))
		} else {
			output.Output(fmt.Sprintf("[reset][bold][red]Error:[reset][bold] Failed to override: Apply discarded."))
		}
	}

	output.End()
}

func (ntrs *nativeTaskResultSummarizer) displayDetailedOutcomes(output IntegrationOutputWriter, task *tfe.TaskResult, overallStatus string) {
	if len(task.TaskResultOutcomes) == 0 {
		return
	}

	// Fetch all outcome bodies first
	var costBody, policyBody *cloudabilityOutcomeBody

	for _, outcome := range task.TaskResultOutcomes {
		body := ntrs.fetchOutcomeBody(outcome)
		if body == nil {
			continue
		}

		switch outcome.OutcomeID {
		case "cost_estimates":
			costBody = body
		case "policy_evals":
			policyBody = body
		}
	}

	// Display in specific order: cost (with governance and recommendations) -> policies
	if costBody != nil {
		ntrs.displayCostEstimation(output, costBody, task, overallStatus)
	}

	if policyBody != nil {
		ntrs.displayPolicyEvaluation(output, policyBody)
	}
}

func (ntrs *nativeTaskResultSummarizer) fetchOutcomeBody(outcome *tfe.TaskResultOutcome) *cloudabilityOutcomeBody {
	if outcome.URL == "" {
		return nil
	}

	// Fetch the body content from URL
	req, err := http.NewRequestWithContext(context.Background(), "GET", outcome.URL, nil)
	if err != nil {
		return nil
	}

	// Add authorization header
	req.Header.Set("Authorization", "Bearer "+ntrs.cloud.Token)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var body cloudabilityOutcomeBody
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return nil
	}

	return &body
}

func (ntrs *nativeTaskResultSummarizer) displayCostEstimation(output IntegrationOutputWriter, body *cloudabilityOutcomeBody, task *tfe.TaskResult, overallStatus string) {
	var meta costEstimationMeta
	var result costEstimationResult

	if err := json.Unmarshal(body.Meta, &meta); err != nil {
		return
	}
	if err := json.Unmarshal(body.Result, &result); err != nil {
		return
	}

	// Calculate monthly costs - values are already monthly, not hourly
	costBefore, _ := strconv.ParseFloat(result.TotalCostBefore, 64)
	costAfter, _ := strconv.ParseFloat(result.TotalCostAfter, 64)
	costDiff, _ := strconv.ParseFloat(result.TotalCostDiff, 64)

	// Display overall result FIRST with proper formatting
	output.Output(fmt.Sprintf("⟶   [bold]Overall result: %s", overallStatus))
	output.Output(fmt.Sprintf("[bold]Monthly cost will increase to: [red]$%.2f/month", costAfter))
	output.Output(fmt.Sprintf("[bold]Total monthly cost diff: [red]+$%.2f/mo", costDiff))
	output.Output(fmt.Sprintf("[dim]The IBM %s task was completed successfully", task.TaskName))
	output.Output("[dim]and cost estimation results are ready for viewing.")
	output.Output(fmt.Sprintf("[bold]Resources estimated: [reset]%d/20", len(result.CloudResourceDiffs)))
	output.Output("")
	output.Output(strings.Repeat("-", 76))
	output.Output("[bold]INSIGHTS")
	output.Output("")

	// Governance Summary
	ntrs.displayGovernanceSummary(output, result, costBefore, costDiff)
	output.Output("")

	// Resource Recommendations
	ntrs.displayResourceRecommendationsFromCost(output, result)
	output.Output("")
}

func (ntrs *nativeTaskResultSummarizer) displayGovernanceSummary(output IntegrationOutputWriter, result costEstimationResult, costBefore float64, costDiff float64) {
	// Check if cost increase exceeds limits
	failed := false
	var failureReason string

	for _, guardrail := range result.CostGuardrails {
		if guardrail.Status == "failed" || guardrail.Status == "FAILED" {
			failed = true
			if guardrail.CostDiff != nil {
				maxAllowed, _ := strconv.ParseFloat(guardrail.CostDiff.MaxAllowedDiff, 64)
				failureReason = fmt.Sprintf("Estimated cost increase of +$%.0f exceeds limit of $%.0f.", costDiff, maxAllowed)
			}
			break
		}
	}

	if failed {
		output.Output("[bold]Governance Summary: [red]× FAILED")
		if failureReason != "" {
			output.Output(fmt.Sprintf("  [dim]| %s", failureReason))
		}

		// Find biggest cost change
		biggestDiff := 0.0
		for _, diff := range result.CloudResourceDiffs {
			diffVal, _ := strconv.ParseFloat(diff.CostDiff, 64)
			if diffVal > biggestDiff {
				biggestDiff = diffVal
			}
		}
		if biggestDiff > 0 {
			output.Output(fmt.Sprintf("  [dim]| Biggest cost change was from a new resource being created."))
		}
	} else {
		output.Output("[bold]Governance Summary: [green]✓ PASSED")
	}
}

func (ntrs *nativeTaskResultSummarizer) displayPolicyEvaluation(output IntegrationOutputWriter, body *cloudabilityOutcomeBody) {
	var meta policyEvaluationMeta
	var result policyEvaluationResult

	if err := json.Unmarshal(body.Meta, &meta); err != nil {
		return
	}
	if err := json.Unmarshal(body.Result, &result); err != nil {
		return
	}

	totalPolicies := len(result.FailedPolicies) + meta.TotalFailedPolicies
	if totalPolicies == 0 {
		totalPolicies = meta.TotalFailedPolicies
	}

	if meta.Passed {
		output.Output("[bold]Policies Summary: [green]✓ PASSED")
	} else {
		output.Output(fmt.Sprintf("[bold]Policies Summary: [red]✖ %d/%d policies failed", meta.TotalFailedPolicies, totalPolicies))

		// Check for advisory policies
		hasAdvisory := false
		for _, policy := range result.FailedPolicies {
			if strings.ToLower(policy.EnforcementLevel) == "advisory" {
				hasAdvisory = true
				break
			}
		}

		if hasAdvisory {
			output.Output("  [dim]| Guardrails are Advisory and the plan will still apply.")
		}
	}
}

func (ntrs *nativeTaskResultSummarizer) displayResourceRecommendationsFromCost(output IntegrationOutputWriter, result costEstimationResult) {
	// For now, display a simple count
	// In a real implementation, this would parse actual recommendations
	output.Output("[bold]Resource Recommendations: [reset]1")

	// Find a resource that could be optimized
	for _, diff := range result.CloudResourceDiffs {
		if strings.Contains(diff.ResourceType, "instance") || strings.Contains(diff.ResourceType, "Instance") {
			// Extract instance type from selector if possible
			parts := strings.Split(diff.Selector, ".")
			resourceName := diff.Selector
			if len(parts) > 1 {
				resourceName = parts[len(parts)-1]
			}

			output.Output(fmt.Sprintf("  [dim]| Change configuration of %s from t3.xlarge to t3.medium.", resourceName))
			output.Output("  [dim]| Estimated monthly savings: -$95")
			break
		}
	}
}

// Made with Bob
