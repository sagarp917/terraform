// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	tfe "github.com/hashicorp/go-tfe"

	"github.com/hashicorp/terraform/internal/terraform"
)

type taskStages map[tfe.Stage]*tfe.TaskStage

const (
	taskStageBackoffMin = 4000.0
	taskStageBackoffMax = 12000.0
)

const taskStageHeader = `
To view this run in a browser, visit:
https://%s/app/%s/%s/runs/%s
`

type taskStageSummarizer interface {
	// Summarize takes an IntegrationContext, IntegrationOutputWriter for
	// writing output and a pointer to a tfe.TaskStage object as arguments.
	// This function summarizes and outputs the results of the task stage.
	// It returns a boolean which signifies whether we should continue polling
	// for results, an optional message string to print while it is polling
	// and an error if any.
	Summarize(*IntegrationContext, IntegrationOutputWriter, *tfe.TaskStage) (bool, *string, error)
}

func (b *Cloud) runTaskStages(ctx context.Context, client *tfe.Client, runId string) (taskStages, error) {
	taskStages := make(taskStages, 0)
	result, err := client.Runs.ReadWithOptions(ctx, runId, &tfe.RunReadOptions{
		Include: []tfe.RunIncludeOpt{tfe.RunTaskStages},
	})
	if err == nil {
		for _, t := range result.TaskStages {
			if t != nil {
				taskStages[t.Stage] = t
			}
		}
	} else {
		// This error would be expected for older versions of TFE that do not allow
		// fetching task_stages.
		if !strings.HasSuffix(err.Error(), "Invalid include parameter") {
			return taskStages, b.generalError("Failed to retrieve run", err)
		}
	}

	return taskStages, nil
}

// fetchTaskResultOutcomes fetches the outcomes for a task result from the API
func (b *Cloud) fetchTaskResultOutcomes(ctx *IntegrationContext, taskResultID string) ([]*tfe.TaskResultOutcome, error) {
	// Build the full URL
	baseURL := b.client.BaseURL()
	url := fmt.Sprintf("%s/task-results/%s/outcomes", baseURL.String(), taskResultID)

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx.StopContext, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Add authorization header
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	// Make the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Read and parse response
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse JSON API response
	var jsonResp struct {
		Data []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				OutcomeID   string        `json:"outcome-id"`
				Description string        `json:"description"`
				Body        string        `json:"body"`
				URL         string        `json:"url"`
				Tags        []interface{} `json:"tags"`
			} `json:"attributes"`
			Links struct {
				Self string `json:"self"`
				Body string `json:"body"`
			} `json:"links"`
		} `json:"data"`
	}

	if err := json.Unmarshal(bodyBytes, &jsonResp); err != nil {
		return nil, err
	}

	// Convert to TaskResultOutcome objects
	outcomes := make([]*tfe.TaskResultOutcome, len(jsonResp.Data))
	for i, data := range jsonResp.Data {
		// Use the body link as the URL if available
		url := data.Attributes.URL
		if url == "" && data.Links.Body != "" {
			// Convert relative URL to absolute (body link already starts with /api/v2/)
			baseURL := b.client.BaseURL()
			// Remove trailing slash and /api/v2 from baseURL since body link includes it
			baseStr := baseURL.String()
			if len(baseStr) > 0 && baseStr[len(baseStr)-1] == '/' {
				baseStr = baseStr[:len(baseStr)-1]
			}
			// Remove /api/v2 suffix if present
			if len(baseStr) >= 7 && baseStr[len(baseStr)-7:] == "/api/v2" {
				baseStr = baseStr[:len(baseStr)-7]
			}
			url = baseStr + data.Links.Body
		}

		outcomes[i] = &tfe.TaskResultOutcome{
			OutcomeID:   data.Attributes.OutcomeID,
			Description: data.Attributes.Description,
			Body:        data.Attributes.Body,
			URL:         url,
		}
	}

	return outcomes, nil
}

func (b *Cloud) getTaskStageWithAllOptions(ctx *IntegrationContext, stageID string) (*tfe.TaskStage, error) {
	options := tfe.TaskStageReadOptions{
		Include: []tfe.TaskStageIncludeOpt{tfe.TaskStageTaskResults, tfe.PolicyEvaluationsTaskResults},
	}
	stage, err := b.client.TaskStages.Read(ctx.StopContext, stageID, &options)
	if err != nil {
		return nil, b.generalError("Failed to retrieve task stage", err)
	}

	// Fetch outcomes for native tasks using the outcomes endpoint
	for i, taskResult := range stage.TaskResults {
		if taskResult.TaskCategory == "native" {
			outcomes, err := b.fetchTaskResultOutcomes(ctx, taskResult.ID)
			if err == nil && outcomes != nil {
				stage.TaskResults[i].TaskResultOutcomes = outcomes
			}
		}
	}

	return stage, nil
}

func (b *Cloud) runTaskStage(ctx *IntegrationContext, output IntegrationOutputWriter, stageID string) error {
	var errs error

	// Create our summarizers
	summarizers := make([]taskStageSummarizer, 0)
	ts, err := b.getTaskStageWithAllOptions(ctx, stageID)
	if err != nil {
		return err
	}

	if s := newTaskResultSummarizer(b, ts); s != nil {
		summarizers = append(summarizers, s)
	}

	// Add native task summarizer
	if s := newNativeTaskResultSummarizer(b, ts); s != nil {
		summarizers = append(summarizers, s)
	}

	if s := newPolicyEvaluationSummarizer(b, ts); s != nil {
		summarizers = append(summarizers, s)
	}

	return ctx.Poll(taskStageBackoffMin, taskStageBackoffMax, func(i int) (bool, error) {
		options := tfe.TaskStageReadOptions{
			Include: []tfe.TaskStageIncludeOpt{tfe.TaskStageTaskResults, tfe.PolicyEvaluationsTaskResults},
		}
		stage, err := b.client.TaskStages.Read(ctx.StopContext, stageID, &options)
		if err != nil {
			return false, b.generalError("Failed to retrieve task stage", err)
		}

		// Fetch outcomes for native tasks using the outcomes endpoint
		for i, taskResult := range stage.TaskResults {
			if taskResult.TaskCategory == "native" {
				outcomes, err := b.fetchTaskResultOutcomes(ctx, taskResult.ID)
				if err == nil && outcomes != nil {
					stage.TaskResults[i].TaskResultOutcomes = outcomes
				}
			}
		}

		switch stage.Status {
		case tfe.TaskStagePending:
			// Waiting for it to start
			return true, nil
		case tfe.TaskStageRunning:
			if _, e := processSummarizers(ctx, output, stage, summarizers, errs); e != nil {
				errs = e
			}
			// not a terminal status so we continue to poll
			return true, nil
		// Note: Terminal statuses need to print out one last time just in case
		case tfe.TaskStagePassed:
			ok, e := processSummarizers(ctx, output, stage, summarizers, errs)
			if e != nil {
				errs = e
			}
			if ok {
				if b.CLI != nil {
					b.CLI.Output("------------------------------------------------------------------------")
				}
				return true, nil
			}
		case tfe.TaskStageCanceled, tfe.TaskStageErrored, tfe.TaskStageFailed:
			ok, e := processSummarizers(ctx, output, stage, summarizers, errs)
			if e != nil {
				errs = e
			}
			if ok {
				if b.CLI != nil {
					b.CLI.Output("------------------------------------------------------------------------")
				}
				return true, nil
			}
			return false, fmt.Errorf("Task Stage %s.", stage.Status)
		case tfe.TaskStageAwaitingOverride:
			ok, e := processSummarizers(ctx, output, stage, summarizers, errs)
			if e != nil {
				errs = e
			}
			if ok {
				return true, nil
			}
			cont, err := b.processStageOverrides(ctx, output, stage.ID)
			if err != nil {
				errs = errors.Join(errs, err)
			} else {
				if b.CLI != nil {
					b.CLI.Output("------------------------------------------------------------------------")
				}
				return cont, nil
			}
		case tfe.TaskStageUnreachable:
			return false, nil
		default:
			return false, fmt.Errorf("Invalid Task stage status: %s ", stage.Status)
		}
		return false, errs
	})
}

func processSummarizers(ctx *IntegrationContext, output IntegrationOutputWriter, stage *tfe.TaskStage, summarizers []taskStageSummarizer, errs error) (bool, error) {
	for _, s := range summarizers {
		cont, msg, err := s.Summarize(ctx, output, stage)
		if err != nil {
			errs = errors.Join(errs, err)
			break
		}

		if !cont {
			continue
		}

		// cont is true and we must continue to poll
		if msg != nil {
			output.OutputElapsed(*msg, len(*msg)) // Up to 2 digits are allowed by the max message allocation
		}
		return true, nil
	}
	return false, errs
}

func (b *Cloud) processStageOverrides(context *IntegrationContext, output IntegrationOutputWriter, taskStageID string) (bool, error) {
	if b.CLI != nil {
		b.CLI.Output("--------------------------------\n")
		b.CLI.Output(b.Colorize().Color(fmt.Sprintf("%c%c [bold]Override", Arrow, Arrow)))
	}
	opts := &terraform.InputOpts{
		Id:          fmt.Sprintf("%c%c [bold]Override", Arrow, Arrow),
		Query:       "\nDo you want to override the failed policies?",
		Description: "Only 'override' will be accepted to override.",
	}
	runURL := fmt.Sprintf(taskStageHeader, b.Hostname, b.Organization, context.Op.Workspace, context.Run.ID)
	err := b.confirm(context.StopContext, context.Op, opts, context.Run, "override")
	if err != nil && err != errRunOverridden {
		return false, fmt.Errorf("Failed to override: %w\n%s\n", err, runURL)
	}

	if err != errRunOverridden {
		if _, err = b.client.TaskStages.Override(context.StopContext, taskStageID, tfe.TaskStageOverrideOptions{}); err != nil {
			return false, b.generalError(fmt.Sprintf("Failed to override policy check.\n%s", runURL), err)
		} else {
			return true, nil
		}
	} else {
		output.Output(fmt.Sprintf("The run needs to be manually overridden or discarded.\n%s\n", runURL))
	}
	return false, nil
}
