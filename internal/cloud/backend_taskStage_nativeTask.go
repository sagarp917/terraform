// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package cloud

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	tfe "github.com/hashicorp/go-tfe"
	tfev2models "github.com/hashicorp/go-tfe/v2/api/models"
)

// Tag key constants mirror the keys set by tfc-agent in cli_summary.go.
// They are defined here so the CLI's tag lookups stay in sync with the
// names tfc-agent writes — a single string to update if a key ever changes.
const (
	tagKeyCLIDisplay        = "cli_display"
	tagKeyCLIDisplayTitle   = "cli_display_title"
	tagKeyCLIPrimaryDisplay = "cli_primary_display"
	tagKeyStatus            = "status"
)

// nativeTaskSummarizer renders CLI summary output for any native integration
// task. It reads the generic "cli_display" and "cli_display_title" tags that
// tfc-agent sets on each outcome. All formatting decisions belong to tfc-agent;
// this layer is completely agnostic to vendor logic or integration-specific
// tag/outcome names. With the Atlas tag limit raised to 10, all integrations
// have room to set both tags alongside their own structural tags.
type nativeTaskSummarizer struct {
	cloud    *Cloud
	finished bool
}

func newNativeTaskSummarizer(b *Cloud, ts *tfe.TaskStage) taskStageSummarizer {
	if b.clientV2 == nil || len(ts.TaskResults) == 0 {
		return nil
	}
	return &nativeTaskSummarizer{cloud: b}
}

func (s *nativeTaskSummarizer) Summarize(
	ctx *IntegrationContext,
	output IntegrationOutputWriter,
	stage *tfe.TaskStage,
) (bool, *string, error) {
	if s.finished || !isTerminalTaskStageStatus(stage.Status) {
		return false, nil, nil
	}

	runURL := ""
	if ctx.Run != nil {
		runURL = s.cloud.runURL(ctx.Op.Workspace, ctx.Run.ID)
	}

	for _, taskResult := range stage.TaskResults {
		outcomes, err := fetchTaskResultOutcomes(ctx.StopContext, s.cloud, taskResult.ID)
		if err != nil {
			return false, nil, fmt.Errorf("fetching outcomes for task %q: %w", taskResult.TaskName, err)
		}
		if !renderNativeTaskSummary(output, taskResult, outcomes, runURL) {
			// No cli_display tags yet (tfc-agent not yet deployed with CLI
			// summary support). Fall back to a minimal structural block so the
			// task result is never silently swallowed.
			renderNativeTaskFallback(output, taskResult, outcomes)
		}
	}

	s.finished = true
	return false, nil, nil
}

// fetchTaskResultOutcomes fetches the v2 outcomes for a task result. It
// returns (nil, nil) when the v2 client is unavailable.
func fetchTaskResultOutcomes(ctx context.Context, cloud *Cloud, taskResultID string) (outcomeList, error) {
	if cloud.clientV2 == nil {
		return nil, nil
	}
	response, err := cloud.clientV2.API.
		TaskResults().
		ByTask_result_id(taskResultID).
		Outcomes().
		Get(ctx, nil)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nil
	}
	return response.GetData(), nil
}

// hasNativeCLISummary reports whether this task result is a native integration
// task whose display is owned by nativeTaskSummarizer, so the generic
// taskResultSummarizer can skip it and avoid printing a duplicate block.
//
// A task result is considered native when the v2 outcomes endpoint returns at
// least one outcome — the presence of any outcome means tfc-agent posted a
// structured result and nativeTaskSummarizer will render the INSIGHTS block.
// We do NOT require the cli_display tag here: that tag may be absent on older
// tfc-agent deployments, but the native task's display block is still rendered
// (or gracefully suppressed) by nativeTaskSummarizer.
//
// During the running phase tfc-agent may not yet have posted any outcomes, so
// this function returns false. taskResultSummarizer therefore prints a generic
// progress row for the in-flight task, which is correct: the native INSIGHTS
// block is rendered later by nativeTaskSummarizer once the stage reaches a
// terminal status. taskResultSummarizer caches this result so the endpoint is
// called at most once per task result ID regardless of how many poll ticks fire.
func (b *Cloud) hasNativeCLISummary(ctx context.Context, taskResultID string) bool {
	outcomes, err := fetchTaskResultOutcomes(ctx, b, taskResultID)
	if err != nil {
		return false
	}
	for _, o := range outcomes {
		if o != nil && o.GetAttributes() != nil && o.GetAttributes().GetOutcomeId() != nil {
			return true
		}
	}
	return false
}

// renderNativeTaskFallback renders a minimal block for a native task when no
// outcome carries a cli_display tag. It shows the task name, overall result
// derived from the task-result status, and a brief per-outcome INSIGHTS list
// using outcome_id and the status tag as the only available data.
func renderNativeTaskFallback(output IntegrationOutputWriter, taskResult *tfe.TaskResult, outcomes outcomeList) {
	output.Output("------------------------------------------------------------------------")
	output.Output(fmt.Sprintf("[bold]%s[reset]", taskResult.TaskName))
	output.Output("Task complete.")
	output.Output("")

	passed := taskResult.Status == tfe.TaskPassed
	if passed {
		output.Output(fmt.Sprintf("%c%c  [bold]Overall result:[reset] [green]%c PASSED[reset]", Arrow, Arrow, Tick))
	} else {
		output.Output(fmt.Sprintf("%c%c  [bold]Overall result:[reset] [red]%c FAILED[reset]", Arrow, Arrow, Cross))
	}

	if len(taskResult.Message) > 0 {
		output.Output("")
		output.Output(fmt.Sprintf("[dim]%s[reset]", taskResult.Message))
	}
	if len(taskResult.URL) > 0 {
		output.Output(fmt.Sprintf("[dim]Details: %s[reset]", taskResult.URL))
	}

	// Build the INSIGHTS list, skipping outcomes with no tags — those are
	// Atlas-pre-created placeholders that tfc-agent never populated.
	var insightOutcomes []tfev2models.TaskResultOutcomesable
	for _, outcome := range outcomes {
		if outcome == nil || outcome.GetAttributes() == nil || outcome.GetAttributes().GetOutcomeId() == nil {
			continue
		}
		if len(outcome.GetAttributes().GetTags()) == 0 {
			continue
		}
		insightOutcomes = append(insightOutcomes, outcome)
	}

	if len(insightOutcomes) > 0 {
		output.Output("")
		output.Output("------------------------------------------------------------------------")
		output.Output("[bold]INSIGHTS[reset]")
		output.Output("")
		for _, outcome := range insightOutcomes {
			attr := outcome.GetAttributes()
			title := displayLabel(*attr.GetOutcomeId())
			status, statusLevel := outcomeStatus(attr.GetTags())
			if status == "" {
				output.Output(fmt.Sprintf("[bold]%s[reset]", title))
			} else {
				symbol := Tick
				if isFailureStatus(status) {
					symbol = Cross
				}
				output.Output(fmt.Sprintf("[bold]%s:[reset] %s%c %s[reset]", title, colorForStatus(status, statusLevel), symbol, strings.ToUpper(status)))
			}
			if detailsURL := attr.GetUrl(); detailsURL != nil && *detailsURL != "" {
				output.SubOutput(fmt.Sprintf("[dim]Details: %s[reset]", *detailsURL))
			}
			output.Output("")
		}
	}

	output.Output("------------------------------------------------------------------------")
}

// renderNativeTaskSummary produces a self-contained CLI summary block for a
// native integration task.
//
// An outcome is included when its "cli_display" tag carries at least one
// value. Outcomes without that tag are silently skipped; integrations that
// have not opted in are unaffected and the block is suppressed entirely when
// no outcome has cli_display data.
//
// Layout:
//
//	------------------------------------------------------------------------
//	<TaskName>
//	Task complete.
//
//	→→  Overall result: × FAILED  (or ✓ PASSED)
//
//	------------------------------------------------------------------------
//	INSIGHTS
//
//	<Title>: <suffix>                    ← heading, one per outcome
//	  | <remaining display lines>
//
//	------------------------------------------------------------------------
func renderNativeTaskSummary(output IntegrationOutputWriter, taskResult *tfe.TaskResult, outcomes outcomeList, runURL string) bool {
	type displayOutcome struct {
		attr         tfev2models.TaskResultOutcomes_attributesable
		title        string
		lines        []tfev2models.TaskResultOutcomes_attributes_tags_valueable
		primaryLines []tfev2models.TaskResultOutcomes_attributes_tags_valueable
	}

	// Single pass: collect display lines, primary lines, and title per outcome.
	var displayOutcomes []displayOutcome
	for _, outcome := range outcomes {
		if outcome == nil || outcome.GetAttributes() == nil {
			continue
		}
		attr := outcome.GetAttributes()
		if attr.GetOutcomeId() == nil {
			continue
		}
		lines := cliDisplayLines(attr.GetTags())
		if len(lines) == 0 {
			// No cli_display tag — skip this outcome entirely.
			continue
		}
		displayOutcomes = append(displayOutcomes, displayOutcome{
			attr:         attr,
			title:        cliDisplayTitle(attr.GetTags()),
			lines:        lines,
			primaryLines: cliPrimaryDisplayLines(attr.GetTags()),
		})
	}
	if len(displayOutcomes) == 0 {
		return false
	}

	// cli.Ui.Output() appends its own newline — never add \n to Output() calls.

	output.Output("------------------------------------------------------------------------")
	output.Output(fmt.Sprintf("[bold]%s[reset]", taskResult.TaskName))
	output.Output("Task complete.")
	output.Output("")

	// Overall pass/fail from the "status" tag across all outcomes.
	overallPassed := true
	for _, do := range displayOutcomes {
		status, _ := outcomeStatus(do.attr.GetTags())
		if isFailureStatus(status) {
			overallPassed = false
			break
		}
	}
	if overallPassed {
		output.Output(fmt.Sprintf("%c%c  [bold]Overall result:[reset] [green]%c PASSED[reset]", Arrow, Arrow, Tick))
	} else {
		output.Output(fmt.Sprintf("%c%c  [bold]Overall result:[reset] [red]%c FAILED[reset]", Arrow, Arrow, Cross))
	}

	// Render primary metrics (e.g. cost_estimates) directly below Overall result.
	for _, do := range displayOutcomes {
		for _, v := range do.primaryLines {
			if v == nil || v.GetLabel() == nil {
				continue
			}
			level := ""
			if v.GetLevel() != nil {
				level = *v.GetLevel()
			}
			for _, line := range strings.Split(*v.GetLabel(), "\n") {
				if level != "" && level != "none" {
					output.Output(colorForLevel(level) + line + "[reset]")
				} else {
					output.Output(formatPrimaryLine(line))
				}
			}
		}
	}

	output.Output("")
	output.Output("------------------------------------------------------------------------")
	output.Output("[bold]INSIGHTS[reset]")
	output.Output("")

	for _, do := range displayOutcomes {
		output.Output(formatOutcomeHeading(do.attr, do.title, do.lines))
		// Remaining lines (index 1+) rendered as indented sub-output.
		renderTagValueLines(output, do.lines[1:])
		if detailsURL := do.attr.GetUrl(); detailsURL != nil && *detailsURL != "" {
			output.SubOutput(fmt.Sprintf("[dim]Details: %s[reset]", *detailsURL))
		}
		output.Output("")
	}

	output.Output("------------------------------------------------------------------------")

	if runURL != "" {
		output.Output("")
		output.SubOutput("To view this run in a browser, visit:")
		output.SubOutput(fmt.Sprintf("[dim]%s[reset]", runURL))
		output.Output("")
	}

	return true
}

// findTagByLabel returns the first tag whose label matches name
// (case-insensitive). Returns nil when no match is found.
func findTagByLabel(tags []tfev2models.TaskResultOutcomes_attributes_tagsable, name string) tfev2models.TaskResultOutcomes_attributes_tagsable {
	for _, tag := range tags {
		if tag != nil && tag.GetLabel() != nil && strings.EqualFold(*tag.GetLabel(), name) {
			return tag
		}
	}
	return nil
}

// cliPrimaryDisplayLines returns the values of the "cli_primary_display" tag in order.
// Returns nil when no such tag exists.
func cliPrimaryDisplayLines(tags []tfev2models.TaskResultOutcomes_attributes_tagsable) []tfev2models.TaskResultOutcomes_attributes_tags_valueable {
	if tag := findTagByLabel(tags, tagKeyCLIPrimaryDisplay); tag != nil {
		return tag.GetValue()
	}
	return nil
}

// cliDisplayLines returns the values of the "cli_display" tag in order.
// Returns nil when no such tag exists.
func cliDisplayLines(tags []tfev2models.TaskResultOutcomes_attributes_tagsable) []tfev2models.TaskResultOutcomes_attributes_tags_valueable {
	if tag := findTagByLabel(tags, tagKeyCLIDisplay); tag != nil {
		return tag.GetValue()
	}
	return nil
}

// cliDisplayTitle reads the "cli_display_title" tag and returns its first value label.
// Returns "" when the tag is absent or carries no values.
func cliDisplayTitle(tags []tfev2models.TaskResultOutcomes_attributes_tagsable) string {
	if tag := findTagByLabel(tags, tagKeyCLIDisplayTitle); tag != nil {
		for _, v := range tag.GetValue() {
			if v != nil && v.GetLabel() != nil {
				return *v.GetLabel()
			}
		}
	}
	return ""
}

// renderTagValueLines writes tag value lines (index 1+) as │-indented
// sub-output via SubOutput, consistent with all other summarizers.
// Each value's Level field controls terminal colour.
func renderTagValueLines(output IntegrationOutputWriter, lines []tfev2models.TaskResultOutcomes_attributes_tags_valueable) {
	for _, v := range lines {
		if v == nil || v.GetLabel() == nil {
			continue
		}
		level := ""
		if v.GetLevel() != nil {
			level = *v.GetLevel()
		}
		if color := colorForLevel(level); color != "" {
			output.SubOutput(fmt.Sprintf("%s%s[reset]", color, *v.GetLabel()))
		} else {
			output.SubOutput(*v.GetLabel())
		}
	}
}

// formatOutcomeHeading builds the INSIGHTS heading line for an outcome.
//
// Title: the "cli_display_title" tag value → fallback displayLabel(outcome_id).
// Suffix: the first cli_display value label, so counts and status text set by
// tfc-agent appear inline, e.g. "Policies Summary: 2 policies failed",
// "Resource Recommendations: 1". A × or ✓ symbol is prepended when the
// outcome carries a "status" tag.
func formatOutcomeHeading(
	attributes tfev2models.TaskResultOutcomes_attributesable,
	title string,
	lines []tfev2models.TaskResultOutcomes_attributes_tags_valueable,
) string {
	if title == "" {
		outcomeID := ""
		if attributes.GetOutcomeId() != nil {
			outcomeID = *attributes.GetOutcomeId()
		}
		title = displayLabel(outcomeID)
	}

	suffix := ""
	suffixLevel := ""
	if len(lines) > 0 && lines[0] != nil && lines[0].GetLabel() != nil {
		suffix = *lines[0].GetLabel()
		if lines[0].GetLevel() != nil {
			suffixLevel = *lines[0].GetLevel()
		}
	}

	status, statusLevel := outcomeStatus(attributes.GetTags())
	if status == "" {
		if suffix == "" {
			return fmt.Sprintf("[bold]%s[reset]", title)
		}
		return fmt.Sprintf("[bold]%s:[reset] %s%s[reset]", title, colorForLevel(suffixLevel), suffix)
	}
	symbol := Tick
	if isFailureStatus(status) {
		symbol = Cross
	}
	if suffix == "" {
		return fmt.Sprintf("[bold]%s:[reset] %s%c %s[reset]", title, colorForStatus(status, statusLevel), symbol, strings.ToUpper(status))
	}
	return fmt.Sprintf("[bold]%s:[reset] %s%c %s[reset]", title, colorForStatus(status, statusLevel), symbol, suffix)
}

// outcomeStatus finds the "status" tag and returns its first value label and level.
func outcomeStatus(tags []tfev2models.TaskResultOutcomes_attributes_tagsable) (string, string) {
	if tag := findTagByLabel(tags, tagKeyStatus); tag != nil {
		for _, value := range tag.GetValue() {
			if value == nil || value.GetLabel() == nil {
				continue
			}
			level := ""
			if value.GetLevel() != nil {
				level = *value.GetLevel()
			}
			return *value.GetLabel(), level
		}
	}
	return "", ""
}

func isFailureStatus(status string) bool {
	s := strings.ToLower(status)
	return s == "failed" || s == "error" || s == "errored"
}

func colorForLevel(level string) string {
	switch strings.ToLower(level) {
	case "error":
		return "[red]"
	case "warning":
		return "[yellow]"
	case "info":
		return "[cyan]"
	case "success":
		return "[green]"
	case "dim":
		return "[dim]"
	case "bold":
		return "[bold]"
	default:
		return ""
	}
}

func colorForStatus(status, level string) string {
	if color := colorForLevel(level); color != "" {
		return color
	}
	if isFailureStatus(status) {
		return "[red]"
	}
	if strings.EqualFold(status, "passed") {
		return "[green]"
	}
	return ""
}

// formatPrimaryLine renders a primary display line (level=none) with bold text.
// When the line contains ": " and the value portion starts with a currency
// symbol or sign character, the value is additionally coloured red so that
// cost figures stand out while the label stays bold-white.
//
// Every colour/attribute tag is followed by [reset] before the next tag so
// that terminals that do not stack SGR codes still render correctly.
func formatPrimaryLine(line string) string {
	idx := strings.Index(line, ": ")
	if idx < 0 {
		return "[bold]" + line + "[reset]"
	}
	label := line[:idx+2] // includes ": "
	value := line[idx+2:]
	if len(value) > 0 && (value[0] == '$' || value[0] == '+' || value[0] == '-') {
		return "[bold]" + label + "[reset][red]" + value + "[reset]"
	}
	return "[bold]" + line + "[reset]"
}

// displayLabel converts a snake_case or kebab-case identifier into a
// title-cased human-readable label, e.g. "policy_evals" → "Policy Evals".
func displayLabel(label string) string {
	label = strings.NewReplacer("_", " ", "-", " ").Replace(label)
	words := strings.Fields(label)
	for i, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

type outcomeList = []tfev2models.TaskResultOutcomesable
